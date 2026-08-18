package incident

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"agent_space/utils/queue"
)

// Read-only proof against a running static-analysis database:
//
//	ANALYSIS_CONTEXT_LIVE_TEST=1 ANALYSIS_DATABASE_URL=... go test ./incident -run LiveAnalysis -v
func TestLiveAnalysisResolverReadsStaticSnapshot(t *testing.T) {
	if os.Getenv("ANALYSIS_CONTEXT_LIVE_TEST") == "" {
		t.Skip("set ANALYSIS_CONTEXT_LIVE_TEST=1 to run against ANALYSIS_DATABASE_URL")
	}
	dsn := os.Getenv("ANALYSIS_DATABASE_URL")
	if dsn == "" {
		t.Fatal("ANALYSIS_DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const newest = `
		SELECT i.region, i.tenant_id, i.investigation_id, i.incident_id,
		       f.service_id, f.environment, f.severity,
		       c.classification, c.version, c.snapshot
		FROM investigations AS i
		JOIN incident_families AS f
		  ON f.region=i.region AND f.tenant_id=i.tenant_id AND f.incident_id=i.incident_id
		JOIN investigation_contexts AS c
		  ON c.region=i.region AND c.tenant_id=i.tenant_id
		 AND c.investigation_id=i.investigation_id
		ORDER BY c.created_at DESC
		LIMIT 1`

	var (
		a        queue.Assignment
		snapshot []byte
	)
	err = pool.QueryRow(ctx, newest).Scan(
		&a.Region, &a.TenantID, &a.InvestigationID, &a.IncidentID,
		&a.ServiceID, &a.Environment, &a.Severity,
		&a.Classification, &a.ContextVersion, &snapshot,
	)
	if err != nil {
		t.Fatalf("read newest assignment context: %v", err)
	}
	root, err := decodeSafeValue(snapshot, 1)
	if err != nil || root.kind != "map" || root.object["generation"].kind != "int" {
		t.Fatalf("read generation from safe snapshot: root=%q err=%v", root.kind, err)
	}
	a.IncidentGen = root.object["generation"].int

	resolver := NewAnalysisResolver(pool, queue.Boundary{
		Region: a.Region, TenantID: a.TenantID, Classification: a.Classification,
	})
	got, err := resolver.Resolve(ctx, a)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.IncidentID != a.IncidentID || got.ContextVersion != a.ContextVersion ||
		got.ServiceID != a.ServiceID || got.Environment != a.Environment || got.Severity != a.Severity {
		t.Fatalf("resolved context drifted from assignment: assignment=%+v context=%+v", a, got)
	}
	if got.Summary == "" || got.LogExcerpt == "" || got.DetectedAt.IsZero() {
		t.Fatalf("resolved context is incomplete: %+v", got)
	}
}
