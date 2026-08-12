package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type columnContract struct {
	name     string
	typeName string
	nullable bool
}

type tableContract struct {
	columns     []columnContract
	constraints []string
	indexes     []string
}

var requiredSchema = map[string]tableContract{
	"schema_migrations": {
		columns: columns("version INT8 false", "name STRING false", "checksum BYTES false", "catalog_checksum BYTES true", "applied_at TIMESTAMPTZ false"),
		indexes: []string{"schema_migrations_pkey"},
	},
	"records_seen": {
		columns:     columns("record_id STRING false", "region STRING false", "tenant_id STRING false", "identity_version STRING false", "source_type STRING false", "semantic_digest BYTES false", "semantic_priority INT2 false", "replay_identity_version STRING false", "first_processed_at TIMESTAMPTZ false", "incident_id STRING true", "generation INT8 true", "safe_outcome STRING false"),
		constraints: []string{"records_seen_scope_nonempty", "records_seen_record_id_shape", "records_seen_incident_id_shape", "records_seen_semantic_digest_shape", "records_seen_semantic_priority", "records_seen_generation_positive"},
		indexes:     []string{"records_seen_pkey", "records_seen_by_scope"},
	},
	"incident_families": {
		columns:     columns("region STRING false", "tenant_id STRING false", "incident_id STRING false", "source_account STRING false", "service_id STRING false", "environment STRING false", "fingerprint_version STRING false", "fingerprint STRING false", "severity STRING false", "detection_status STRING false", "latest_generation INT8 true", "latest_occurrence TIMESTAMPTZ true", "active_investigation_id STRING true", "context_version INT8 false", "created_at TIMESTAMPTZ false"),
		constraints: []string{"incident_families_incident_id_shape", "incident_families_source_account_nonempty", "incident_families_natural_key", "incident_families_status", "incident_families_context_nonnegative", "incident_families_latest_generation_positive", "incident_families_active_investigation_fk"},
		indexes:     []string{"incident_families_pkey", "incident_families_natural_key"},
	},
	"incident_generations": {
		columns:     columns("region STRING false", "tenant_id STRING false", "incident_id STRING false", "generation INT8 false", "deployment_id STRING false", "episode_start TIMESTAMPTZ false", "first_occurrence TIMESTAMPTZ false", "latest_occurrence TIMESTAMPTZ false", "occurrence_count INT8 false", "severity STRING false", "detection_status STRING false", "quiet_at TIMESTAMPTZ false", "reopen_until TIMESTAMPTZ false", "rule_trigger STRING false", "context_version INT8 false"),
		constraints: []string{"incident_generations_episode_key", "incident_generations_family_fk", "incident_generations_positive", "incident_generations_status", "incident_generations_rule_trigger", "incident_generations_times"},
		indexes:     []string{"incident_generations_pkey", "incident_generations_episode_key"},
	},
	"occurrences": {
		columns:     columns("region STRING false", "tenant_id STRING false", "record_id STRING false", "incident_id STRING false", "generation INT8 false", "event_time TIMESTAMPTZ false", "observed_time TIMESTAMPTZ false", "fingerprint_version STRING false", "fingerprint STRING false", "safe_summary JSONB false", "trace_token STRING true", "span_token STRING true", "late BOOL false", "evidence_only BOOL false", "regional_evidence_reference JSONB true"),
		constraints: []string{"occurrences_record_id_unique", "occurrences_generation_fk"},
		indexes:     []string{"occurrences_pkey", "occurrences_record_id_unique", "occurrences_rule_window"},
	},
	"evidence": {
		columns:     columns("region STRING false", "tenant_id STRING false", "evidence_id STRING false", "incident_id STRING false", "generation INT8 false", "version INT8 false", "classification STRING false", "safe_payload JSONB false", "regional_reference JSONB true", "provenance STRING false", "created_at TIMESTAMPTZ false", "expires_at TIMESTAMPTZ false"),
		constraints: []string{"evidence_id_uuidv7", "evidence_generation_fk", "evidence_version_positive", "evidence_classification", "evidence_provenance", "evidence_expiry"},
		indexes:     []string{"evidence_pkey"},
	},
	"investigations": {
		columns:     columns("region STRING false", "tenant_id STRING false", "investigation_id STRING false", "incident_id STRING false", "state STRING false", "trigger_reason STRING false", "context_version INT8 false", "report_reference STRING true", "queued_at TIMESTAMPTZ false", "started_at TIMESTAMPTZ true", "completed_at TIMESTAMPTZ true"),
		constraints: []string{"investigations_id_uuidv7", "investigations_family_pair_unique", "investigations_family_fk", "investigations_context_positive", "investigations_state"},
		indexes:     []string{"investigations_pkey", "investigations_family_pair_unique"},
	},
	"investigation_claims": {
		columns:     columns("region STRING false", "tenant_id STRING false", "incident_id STRING false", "investigation_id STRING false", "owner STRING true", "lease_token STRING true", "lease_expires_at TIMESTAMPTZ true", "renewal_sequence INT8 false"),
		constraints: []string{"investigation_claims_investigation_unique", "investigation_claims_family_fk", "investigation_claims_investigation_fk", "investigation_claims_lease_shape", "investigation_claims_token_uuidv7", "investigation_claims_renewal_nonnegative"},
		indexes:     []string{"investigation_claims_pkey", "investigation_claims_investigation_unique"},
	},
	"investigation_progress": {
		columns:     columns("region STRING false", "tenant_id STRING false", "investigation_id STRING false", "sequence INT8 false", "state STRING false", "safe_payload JSONB false", "created_at TIMESTAMPTZ false"),
		constraints: []string{"investigation_progress_investigation_fk", "investigation_progress_sequence_positive"},
		indexes:     []string{"investigation_progress_pkey"},
	},
	"investigation_contexts": {
		columns:     columns("region STRING false", "tenant_id STRING false", "investigation_id STRING false", "version INT8 false", "snapshot JSONB false", "classification STRING false", "policy_version STRING false", "created_at TIMESTAMPTZ false"),
		constraints: []string{"investigation_contexts_investigation_fk", "investigation_contexts_version_positive", "investigation_contexts_classification"},
		indexes:     []string{"investigation_contexts_pkey"},
	},
	"reports": {
		columns:     columns("region STRING false", "tenant_id STRING false", "report_id STRING false", "investigation_id STRING false", "structured_report JSONB false", "classification STRING false", "provenance STRING false", "completed_at TIMESTAMPTZ false", "expires_at TIMESTAMPTZ false", "embedding_reference STRING true"),
		constraints: []string{"reports_id_uuidv7", "reports_investigation_unique", "reports_investigation_fk", "reports_classification", "reports_expiry"},
		indexes:     []string{"reports_pkey", "reports_investigation_unique"},
	},
	"outbox_messages": {
		columns:     columns("region STRING false", "tenant_id STRING false", "message_id STRING false", "deduplication_key STRING false", "message_type STRING false", "aggregate_type STRING false", "aggregate_id STRING false", "payload_version STRING false", "payload BYTES false", "attributes JSONB false", "content_digest BYTES false", "state STRING false", "attempts INT8 false", "next_attempt_at TIMESTAMPTZ false", "claim_owner STRING true", "claim_token STRING true", "claim_expires_at TIMESTAMPTZ true", "created_at TIMESTAMPTZ false", "published_at TIMESTAMPTZ true", "last_safe_error STRING true"),
		constraints: []string{"outbox_messages_id_uuidv7", "outbox_messages_claim_token_uuidv7", "outbox_messages_deduplication_unique", "outbox_messages_content_digest_length", "outbox_messages_state", "outbox_messages_attempts_nonnegative", "outbox_messages_claim_shape", "outbox_messages_published_shape"},
		indexes:     []string{"outbox_messages_pkey", "outbox_messages_deduplication_unique", "outbox_messages_claimable"},
	},
	"audit_events": {
		columns:     columns("region STRING false", "tenant_id STRING false", "audit_id STRING false", "actor STRING false", "action STRING false", "target_type STRING false", "target_id STRING false", "safe_details JSONB false", "source_identity STRING false", "source_address STRING true", "created_at TIMESTAMPTZ false"),
		constraints: []string{"audit_events_id_uuidv7"},
		indexes:     []string{"audit_events_pkey"},
	},
}

func liveCatalogChecksum(ctx context.Context, pool *pgxpool.Pool) ([sha256.Size]byte, error) {
	tables := make([]string, 0, len(requiredSchema))
	for table := range requiredSchema {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	hash := sha256.New()
	for _, table := range tables {
		var returnedName, createStatement string
		quoted := fmt.Sprintf(`%q`, table)
		if err := pool.QueryRow(ctx, `SHOW CREATE TABLE `+quoted).Scan(&returnedName, &createStatement); err != nil {
			return [sha256.Size]byte{}, err
		}
		for _, value := range []string{returnedName, createStatement} {
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(value)))
			_, _ = hash.Write(size[:])
			_, _ = hash.Write([]byte(value))
		}
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

type singleRegionTopology uint8

const (
	topologyIncompatible singleRegionTopology = iota
	topologyPortable
	topologyCockroachRegional
)

func inspectSingleRegionTopology(ctx context.Context, pool *pgxpool.Pool) (singleRegionTopology, error) {
	var primaryRegion, secondaryRegion, onlyRegion, survivalGoal *string
	var regionCount int
	err := pool.QueryRow(ctx, `SELECT primary_region, secondary_region,
		COALESCE(array_length(regions, 1), 0), regions[1], survival_goal
		FROM [SHOW DATABASES] WHERE database_name=current_database()`).Scan(
		&primaryRegion, &secondaryRegion, &regionCount, &onlyRegion, &survivalGoal)
	if err != nil {
		return topologyIncompatible, err
	}
	return classifySingleRegionTopology(primaryRegion, secondaryRegion, regionCount, onlyRegion, survivalGoal), nil
}

func classifySingleRegionTopology(primaryRegion, secondaryRegion *string, regionCount int, onlyRegion, survivalGoal *string) singleRegionTopology {
	primary := trimmed(primaryRegion)
	secondary := trimmed(secondaryRegion)
	region := trimmed(onlyRegion)
	survival := trimmed(survivalGoal)
	switch {
	case primary == "" && secondary == "" && regionCount == 0 && region == "" && survival == "":
		return topologyPortable
	case primary != "" && secondary == "" && regionCount == 1 && region == primary && survival == "zone":
		return topologyCockroachRegional
	default:
		return topologyIncompatible
	}
}

func trimmed(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func verifySingleRegionTopology(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	topology, err := inspectSingleRegionTopology(ctx, pool)
	return topology != topologyIncompatible, err
}

func columns(specs ...string) []columnContract {
	result := make([]columnContract, 0, len(specs))
	for _, spec := range specs {
		fields := strings.Fields(spec)
		result = append(result, columnContract{name: fields[0], typeName: fields[1], nullable: fields[2] == "true"})
	}
	return result
}

func verifyLiveSchema(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	tables := make([]string, 0, len(requiredSchema))
	for table := range requiredSchema {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		contract := requiredSchema[table]
		var found bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables
			WHERE table_schema='public' AND table_name=$1)`, table).Scan(&found); err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		quoted := fmt.Sprintf(`%q`, table)
		rows, err := pool.Query(ctx, `SELECT column_name,data_type,is_nullable FROM [SHOW COLUMNS FROM `+quoted+`]`)
		if err != nil {
			return false, err
		}
		actualColumns := map[string]columnContract{}
		for rows.Next() {
			var column columnContract
			if err := rows.Scan(&column.name, &column.typeName, &column.nullable); err != nil {
				rows.Close()
				return false, err
			}
			actualColumns[column.name] = column
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return false, err
		}
		rows.Close()
		if len(actualColumns) != len(contract.columns) {
			return false, nil
		}
		for _, expected := range contract.columns {
			actual, ok := actualColumns[expected.name]
			if !ok || actual.typeName != expected.typeName || actual.nullable != expected.nullable {
				return false, nil
			}
		}
		for _, query := range []struct {
			field    string
			required []string
		}{
			{field: "constraint_name", required: contract.constraints},
			{field: "index_name", required: contract.indexes},
		} {
			if len(query.required) == 0 {
				continue
			}
			statement := `SELECT DISTINCT ` + query.field + ` FROM [SHOW `
			if query.field == "constraint_name" {
				statement += `CONSTRAINTS`
			} else {
				statement += `INDEXES`
			}
			statement += ` FROM ` + quoted + `]`
			rows, err := pool.Query(ctx, statement)
			if err != nil {
				return false, err
			}
			actual := map[string]bool{}
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					rows.Close()
					return false, err
				}
				actual[name] = true
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return false, err
			}
			rows.Close()
			for _, name := range query.required {
				if !actual[name] {
					return false, nil
				}
			}
		}
	}
	return true, nil
}
