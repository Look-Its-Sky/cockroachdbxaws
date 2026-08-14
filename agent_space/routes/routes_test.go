package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"agent_space/utils"
)

// the route set as main.go registers it. Gin panics on a conflicting pattern at
// registration rather than at request time, so a bad combination is a crash at
// boot — which is exactly the sort of thing that is found during a demo.
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

	guarded.GET("/repositories", Repositories)
	guarded.GET("/remediations", Remediations)
	guarded.GET("/agent/:id/remediation", RemediationFor)
	guarded.POST("/agent/:id/remediation", StartRemediation)
	guarded.POST("/solutions/:candidate/pr", OpenPullRequest)

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
		"POST /solutions/:candidate/pr",
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
