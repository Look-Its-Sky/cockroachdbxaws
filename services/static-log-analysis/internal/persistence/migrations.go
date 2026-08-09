package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/migrations"
	crdbpgxv5 "github.com/cockroachdb/cockroach-go/v2/crdb/crdbpgxv5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const CurrentSchemaVersion = 2

// expectedCatalogChecksumHex pins CockroachDB v25.3.7 SHOW CREATE output for
// the complete reviewed single-region schema, including defaults, constraint
// definitions, index columns/uniqueness, FKs, and locality.
const expectedCatalogChecksumHex = "07edf27ac7f72c991a640d4ded3faf3f99ffafbe88add3b2db98fb26c9416792"

type migration struct {
	version  int
	name     string
	checksum [sha256.Size]byte
	sql      string
}

func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, topology Topology) error {
	if pool == nil || topology != TopologySingleRegion {
		return ErrInvalidInput
	}
	compatibleTopology, err := verifySingleRegionTopology(ctx, pool)
	if err != nil {
		return databaseError(ctx, err)
	}
	if !compatibleTopology {
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
		if err != nil {
			if errors.Is(err, ErrIncompatibleSchema) {
				return ErrIncompatibleSchema
			}
			return databaseError(ctx, err)
		}
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
	if hex.EncodeToString(catalogChecksum[:]) != expectedCatalogChecksumHex {
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
		result = append(result, migration{version: version, name: entry.Name(), checksum: sha256.Sum256(content), sql: string(content)})
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
