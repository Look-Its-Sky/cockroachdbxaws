package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"agent_space/remediation"
	"agent_space/utils"
)

// the route set as main.go registers it; Gin panics on a conflicting pattern at
// registration, so a bad combination is a crash at boot
func router() *gin.Engine {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(utils.CORS())

	guarded := r.Group("", utils.RequireToken())

	r.GET("/ping", utils.Ping)
	r.GET("/tools", Tools)

	guarded.POST("/store", Store)
	guarded.POST("/retrieve", Retrieve)
	guarded.POST("/ask", Ask)
	guarded.POST("/agent", Agent)
	guarded.GET("/agent/:id", Investigation)

	guarded.GET("/capabilities", Capabilities)
	guarded.GET("/repositories", Repositories)
	guarded.GET("/remediations", Remediations)
	guarded.GET("/agent/:id/remediation", RemediationFor)
	guarded.POST("/agent/:id/remediation", StartRemediation)
	guarded.POST("/solutions/:candidate/pr", OpenPullRequest)
	guarded.POST("/agent/:id/decision", RecordDecision)

	return r
}

func TestRoutesRegisterWithoutConflicting(t *testing.T) {
	// a panic here is the failure; reaching the assertion is most of the test
	r := router()

	registered := make(map[string]bool)
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	for _, want := range []string{
		"POST /agent",
		"GET /agent/:id",
		"GET /agent/:id/remediation",
		"POST /agent/:id/remediation",
		"POST /agent/:id/decision",
		"POST /solutions/:candidate/pr",
		"GET /capabilities",
		"GET /repositories",
		"GET /remediations",
	} {
		if !registered[want] {
			t.Errorf("%s was not registered", want)
		}
	}
}

// with nothing wired up, every remediation route has to say which prerequisite
// is missing rather than panicking on a nil store
func TestRemediationRoutesReportWhatIsMissing(t *testing.T) {
	Remediation, Solutions, Repos, Publisher, Results, Incidents = nil, nil, nil, nil, nil, nil

	r := router()

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/repositories"},
		{http.MethodGet, "/remediations"},
		{http.MethodGet, "/agent/inv-1/remediation"},
		{http.MethodPost, "/agent/inv-1/remediation"},
		{http.MethodPost, "/solutions/cand-1/pr"},
		{http.MethodPost, "/agent/inv-1/decision"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))

			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503 with an explanation; body: %s", w.Code, w.Body.String())
			}
		})
	}
}

// unlike the routes above, this one must never 503 — its entire point is to
// tell the frontend what is missing before it tries a route that would
func TestCapabilitiesReportsWhatIsMissing(t *testing.T) {
	Remediation, Solutions, Repos, Publisher, Results, Incidents = nil, nil, nil, nil, nil, nil

	r := router()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/capabilities", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var body map[string]struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, key := range []string{
		"repositories", "remediation", "solutions", "queue_worker", "pr_opening", "precedent_index",
	} {
		got, ok := body[key]
		if !ok {
			t.Errorf("%s missing from the response", key)
			continue
		}
		if got.Available {
			t.Errorf("%s reported available with nothing wired up", key)
		}
		if got.Reason == "" {
			t.Errorf("%s reported unavailable with no reason an engineer could read", key)
		}
	}
}

// a capability the route actually gates (pr_opening, on Publisher.Configured)
// must flip to true once its prerequisite is met, not just report false always
func TestCapabilitiesReflectsWhatIsWired(t *testing.T) {
	Remediation, Solutions, Repos, Results, Incidents = nil, nil, nil, nil, nil
	Publisher = remediation.NewPublisher("a-token")
	defer func() { Publisher = nil }()

	r := router()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/capabilities", nil))

	var body map[string]struct {
		Available bool `json:"available"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !body["pr_opening"].Available {
		t.Errorf("pr_opening reported unavailable with a token configured")
	}
	if body["solutions"].Available {
		t.Errorf("solutions reported available with nothing wired up")
	}
}

// the binding tags carry real validation: without `dive` a rejection with no
// candidate_id is never checked, and the caps stop unbounded text reaching a prompt
func TestDecisionRequestBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "a pick with an explained rejection",
			body: `{"chosen_candidate_id":"cand-1",
			        "rejections":[{"candidate_id":"cand-2","reason":"too broad"}]}`,
		},
		{name: "a bare pick, with nothing typed", body: `{"chosen_candidate_id":"cand-1"}`},
		{
			name: "rejecting everything and describing the fix instead",
			body: `{"engineer_fix":"widened the accumulator",
			        "engineer_pr_url":"https://github.com/example/repo/pull/2"}`,
		},

		// dive: without it this element is never validated
		{
			name:    "a rejection with no candidate id",
			body:    `{"chosen_candidate_id":"cand-1","rejections":[{"reason":"too broad"}]}`,
			wantErr: true,
		},
		{
			name:    "a reason past the cap",
			body:    `{"chosen_candidate_id":"cand-1","rejections":[{"candidate_id":"c","reason":"` + strings.Repeat("x", 1001) + `"}]}`,
			wantErr: true,
		},
		{
			name:    "a note past the cap",
			body:    `{"notes":"` + strings.Repeat("x", 2001) + `"}`,
			wantErr: true,
		},
		{
			name:    "a pull request url that is not a url",
			body:    `{"engineer_fix":"did it by hand","engineer_pr_url":"not a url"}`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/agent/inv-1/decision",
				strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")

			var req DecisionRequest
			err := c.ShouldBindJSON(&req)

			if tc.wantErr && err == nil {
				t.Errorf("binding accepted %s", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("binding rejected a valid body: %v", err)
			}
		})
	}
}
