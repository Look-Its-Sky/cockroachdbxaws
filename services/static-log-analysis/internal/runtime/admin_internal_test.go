package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
)

func TestOverviewEndpointExportsOnlyTheSafeProjection(t *testing.T) {
	admin, err := newAdminServer("127.0.0.1:0", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Shutdown(context.Background())
	admin.overview = func(context.Context) (persistence.Overview, error) {
		return persistence.Overview{
			RecordsSeen: 12, IncidentFamilies: 1,
			Investigations: map[string]int64{"queued": 1},
			Outbox:         map[string]int64{"published": 1},
			Recent: []persistence.OverviewInvestigation{{
				InvestigationID: "0198f293-7a00-7000-8000-000000000001",
				Service:         "payment", Environment: "demo", Severity: "error",
				State: "queued", TriggerReason: "five_in_five", QueuedAt: time.Unix(1, 0).UTC(),
			}},
		}, nil
	}

	response, err := http.Get("http://" + admin.Addr() + OverviewPath)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	var value persistence.Overview
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	if value.RecordsSeen != 12 || len(value.Recent) != 1 || value.Recent[0].Service != "payment" {
		t.Fatalf("unexpected overview: %+v", value)
	}
}

func TestOverviewEndpointIsAbsentWithoutAProvider(t *testing.T) {
	admin, err := newAdminServer("127.0.0.1:0", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Shutdown(context.Background())
	response, err := http.Get("http://" + admin.Addr() + OverviewPath)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", response.StatusCode)
	}
}
