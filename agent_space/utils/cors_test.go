package utils

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func corsRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(CORS())
	r.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	return r
}

func request(t *testing.T, r *gin.Engine, method, path, origin string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCORSAllowsAnyOriginWhenUnconfigured(t *testing.T) {
	t.Setenv("CORS_ORIGINS", "")

	w := request(t, corsRouter(), http.MethodGet, "/ping", "http://localhost:5173")

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("allow-origin = %q, want the requesting origin echoed back", got)
	}
	// the response differs per origin, so a shared cache must not reuse it
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Errorf("vary = %q, want Origin", got)
	}
	if w.Body.String() != "pong" {
		t.Errorf("the handler did not run: %q", w.Body.String())
	}
}

// a preflight carries no body and OPTIONS is not a route any handler serves
func TestCORSAnswersPreflightWithoutReachingAHandler(t *testing.T) {
	t.Setenv("CORS_ORIGINS", "")

	w := request(t, corsRouter(), http.MethodOptions, "/ping", "http://localhost:5173")

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a preflight came back with a body: %q", w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("a preflight did not say which headers are allowed")
	}
}

func TestCORSNarrowsToTheConfiguredList(t *testing.T) {
	t.Setenv("CORS_ORIGINS", "http://localhost:5173, https://app.example.com")

	r := corsRouter()

	if got := request(t, r, http.MethodGet, "/ping", "https://app.example.com").
		Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("a listed origin was not allowed: %q", got)
	}

	// the browser is the one that enforces this, so the request still runs; what
	// matters is that the header is withheld
	if got := request(t, r, http.MethodGet, "/ping", "https://evil.example.com").
		Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unlisted origin was allowed: %q", got)
	}
}

// a same-origin request has nothing to negotiate
func TestCORSIsSilentWithoutAnOrigin(t *testing.T) {
	t.Setenv("CORS_ORIGINS", "")

	w := request(t, corsRouter(), http.MethodGet, "/ping", "")

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q on a request with no Origin", got)
	}
	if w.Body.String() != "pong" {
		t.Errorf("the handler did not run: %q", w.Body.String())
	}
}
