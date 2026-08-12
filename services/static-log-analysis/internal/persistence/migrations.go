package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/migrations"
	crdbpgxv5 "github.com/cockroachdb/cockroach-go/v2/crdb/crdbpgxv5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const CurrentSchemaVersion = 2

const schemaLockRestoreTimeout = 30 * time.Second

const (
	expectedPortableCatalogChecksumV2537       = "07edf27ac7f72c991a640d4ded3faf3f99ffafbe88add3b2db98fb26c9416792"
	expectedPortableCatalogChecksumV2625       = "ff0d8b05d0917ed8c08385723c0d6cd42c52c8499390a9cfb41bf962db7c6179"
	expectedRegionalCatalogChecksumV2537       = "5c4e479a20d33a9af6345a8c4fd1660b569dbf039ec453b8181d27aedb0d28dd"
	expectedRegionalCatalogChecksumV2625       = "40b5fed64ac50f2533971fe14ead21b73af42e893a36b06e394909abbab0bf63"
	expectedLockedRegionalCatalogChecksumV2625 = "2ef14b55a31a86de4099ac20953b32a54c1b643935a81c36fb64ebb49895a0b3"
)

// SHOW CREATE formatting is CockroachDB-version-specific. These closed sets
// pin the complete reviewed schema for supported versions while keeping the
// portable and one-region-managed locality contracts distinct.
var expectedCatalogChecksums = map[singleRegionTopology]map[string]struct{}{
	topologyPortable: {
		expectedPortableCatalogChecksumV2537: {},
		expectedPortableCatalogChecksumV2625: {},
	},
	topologyCockroachRegional: {
		expectedRegionalCatalogChecksumV2537:       {},
		expectedRegionalCatalogChecksumV2625:       {},
		expectedLockedRegionalCatalogChecksumV2625: {},
	},
}

type migration struct {
	version            int
	name               string
	checksum           [sha256.Size]byte
	sql                string
	schemaChangeTables []string
}

var migrationSchemaChangeTables = map[int][]string{
	2: {"occurrences"},
}

func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, topology Topology) error {
	if pool == nil || topology != TopologySingleRegion {
		return ErrInvalidInput
	}
	databaseTopology, err := inspectSingleRegionTopology(ctx, pool)
	if err != nil {
		return databaseError(ctx, err)
	}
	if databaseTopology == topologyIncompatible {
		return ErrIncompatibleSchema
	}
	loaded, err := loadMigrations()
	if err != nil {
		return ErrIncompatibleSchema
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT8 PRIMARY KEY,
		name STRING NOT NULL,
		checksum BYTES NOT NULL,
		catalog_checksum BYTES NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return databaseError(ctx, err)
	}

	var highest int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM schema_migrations`).Scan(&highest); err != nil {
		return databaseError(ctx, err)
	}
	if highest > CurrentSchemaVersion {
		return ErrIncompatibleSchema
	}

	for _, item := range loaded {
		item := item
		var lockedTables []string
		if item.version > highest {
			lockedTables, err = temporarilyUnlockTables(ctx, pool, item.schemaChangeTables)
			if err != nil {
				return databaseError(ctx, err)
			}
		}
		err := crdbpgxv5.ExecuteTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			var name string
			var checksum []byte
			err := tx.QueryRow(ctx, `SELECT name,checksum FROM schema_migrations WHERE version = $1`, item.version).Scan(&name, &checksum)
			switch {
			case err == nil:
				if name != item.name || !equalBytes(checksum, item.checksum[:]) {
					return ErrIncompatibleSchema
				}
				return nil
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
			for _, statement := range splitMigration(item.sql) {
				if _, err := tx.Exec(ctx, statement); err != nil {
					return err
				}
			}
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, item.version, item.name, item.checksum[:])
			return err
		})
		relockErr := restoreTableLocks(ctx, pool, lockedTables)
		if relockErr != nil {
			return databaseError(ctx, relockErr)
		}
		if err != nil {
			if errors.Is(err, ErrIncompatibleSchema) {
				return ErrIncompatibleSchema
			}
			return databaseError(ctx, err)
		}
		highest = item.version
	}
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return databaseError(ctx, err)
	}
	var versions []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return databaseError(ctx, err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return databaseError(ctx, err)
	}
	rows.Close()
	if len(versions) != len(loaded) {
		return ErrIncompatibleSchema
	}
	for i, version := range versions {
		if version != i+1 {
			return ErrIncompatibleSchema
		}
	}
	compatible, err := verifyLiveSchema(ctx, pool)
	if err != nil {
		return databaseError(ctx, err)
	}
	if !compatible {
		return ErrIncompatibleSchema
	}
	catalogChecksum, err := liveCatalogChecksum(ctx, pool)
	if err != nil {
		return databaseError(ctx, err)
	}
	if !catalogChecksumIsExpected(databaseTopology, hex.EncodeToString(catalogChecksum[:])) {
		return ErrIncompatibleSchema
	}
	var storedCatalog []byte
	if err := pool.QueryRow(ctx, `SELECT catalog_checksum FROM schema_migrations WHERE version=$1`,
		CurrentSchemaVersion).Scan(&storedCatalog); err != nil {
		return databaseError(ctx, err)
	}
	if storedCatalog == nil {
		if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET catalog_checksum=$2
			WHERE version=$1 AND catalog_checksum IS NULL`, CurrentSchemaVersion, catalogChecksum[:]); err != nil {
			return databaseError(ctx, err)
		}
	} else if !equalBytes(storedCatalog, catalogChecksum[:]) {
		return ErrIncompatibleSchema
	}
	return nil
}

func temporarilyUnlockTables(ctx context.Context, pool *pgxpool.Pool, tables []string) ([]string, error) {
	locked := make([]string, 0, len(tables))
	for _, table := range tables {
		if _, known := requiredSchema[table]; !known {
			_ = restoreTableLocks(ctx, pool, locked)
			return nil, ErrIncompatibleSchema
		}
		var returnedName, createStatement string
		quoted := fmt.Sprintf(`%q`, table)
		if err := pool.QueryRow(ctx, `SHOW CREATE TABLE `+quoted).Scan(&returnedName, &createStatement); err != nil {
			_ = restoreTableLocks(ctx, pool, locked)
			return nil, err
		}
		if !strings.Contains(createStatement, "schema_locked = true") {
			continue
		}
		if _, err := pool.Exec(ctx, `ALTER TABLE `+quoted+` SET (schema_locked=false)`); err != nil {
			_ = restoreTableLocks(ctx, pool, locked)
			return nil, err
		}
		locked = append(locked, table)
	}
	return locked, nil
}

func restoreTableLocks(ctx context.Context, pool *pgxpool.Pool, tables []string) error {
	cleanupBase := context.Background()
	if ctx != nil {
		cleanupBase = context.WithoutCancel(ctx)
	}
	cleanupCtx, cancel := context.WithTimeout(cleanupBase, schemaLockRestoreTimeout)
	defer cancel()

	var first error
	for i := len(tables) - 1; i >= 0; i-- {
		quoted := fmt.Sprintf(`%q`, tables[i])
		if _, err := pool.Exec(cleanupCtx, `ALTER TABLE `+quoted+` SET (schema_locked=true)`); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func catalogChecksumIsExpected(topology singleRegionTopology, checksum string) bool {
	allowed, ok := expectedCatalogChecksums[topology]
	if !ok {
		return false
	}
	_, ok = allowed[checksum]
	return ok
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		return nil, err
	}
	var result []migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return nil, ErrIncompatibleSchema
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version < 1 {
			return nil, ErrIncompatibleSchema
		}
		content, err := fs.ReadFile(migrations.Files, entry.Name())
		if err != nil {
			return nil, err
		}
		result = append(result, migration{
			version: version, name: entry.Name(), checksum: sha256.Sum256(content), sql: string(content),
			schemaChangeTables: append([]string(nil), migrationSchemaChangeTables[version]...),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].version < result[j].version })
	if len(result) != CurrentSchemaVersion {
		return nil, ErrIncompatibleSchema
	}
	for i, item := range result {
		if item.version != i+1 {
			return nil, ErrIncompatibleSchema
		}
	}
	return result, nil
}

func splitMigration(sql string) []string {
	parts := strings.Split(sql, "-- migrate:split")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if statement := strings.TrimSpace(part); statement != "" {
			result = append(result, statement)
		}
	}
	return result
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for i := range left {
		different |= left[i] ^ right[i]
	}
	return different == 0
}
