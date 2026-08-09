package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The admin surface is separate from the OTLP listener on purpose. operations.md
// requires metrics and health, and putting them on the ingress port would make
// them reachable by whoever can export logs — on the static_local socket, that
// is anyone who can reach the port at all.
//
// Nothing here carries record-derived content. Every metric is a categorical,
// process-lifetime counter with no labels, because operations.md forbids
// unbounded label cardinality and every interesting label in this service is
// unbounded: a service name, a stream name, a fingerprint.
const (
	HealthPath    = "/healthz"
	ReadyPath     = "/readyz"
	MetricsPath   = "/metrics"
	adminReadTime = 5 * time.Second
)

// Health is what a readiness check answers from.
type Health struct {
	// Ready is false while a replica cannot do the work its role exists for.
	Ready bool
	// Reason is a short categorical explanation. It is never record-derived.
	Reason string
}

// metric is one exported counter.
type metric struct {
	name  string
	help  string
	value uint64
}

// adminServer serves health, readiness, and metrics.
type adminServer struct {
	server   *http.Server
	listener net.Listener
	serving  sync.WaitGroup

	health  func() Health
	metrics func() []metric
}

func newAdminServer(listen string, health func() Health, metrics func() []metric) (*adminServer, error) {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("runtime: listen admin on %s: %w", listen, err)
	}
	admin := &adminServer{listener: listener, health: health, metrics: metrics}
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, admin.serveHealth)
	mux.HandleFunc(ReadyPath, admin.serveReady)
	mux.HandleFunc(MetricsPath, admin.serveMetrics)
	admin.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: adminReadTime,
		ReadTimeout:       adminReadTime,
		WriteTimeout:      adminReadTime,
		IdleTimeout:       adminReadTime * 2,
		MaxHeaderBytes:    16 << 10,
	}
	admin.serving.Add(1)
	go func() {
		defer admin.serving.Done()
		if err := admin.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// An admin listener that dies is not a reason to stop serving
			// traffic. Losing metrics is bad; refusing logs because metrics are
			// unavailable would be worse.
			return
		}
	}()
	return admin, nil
}

func (a *adminServer) Addr() string {
	if a == nil || a.listener == nil {
		return ""
	}
	return a.listener.Addr().String()
}

func (a *adminServer) Shutdown(ctx context.Context) {
	if a == nil {
		return
	}
	_ = a.server.Shutdown(ctx)
	_ = a.server.Close()
	a.serving.Wait()
}

// serveHealth answers liveness. It is deliberately unconditional: the process
// is running, which is the only question liveness asks. Conflating it with
// readiness makes a supervisor kill a replica that is merely waiting for a
// dependency, which is exactly when its journal is holding acknowledged work.
func (a *adminServer) serveHealth(writer http.ResponseWriter, _ *http.Request) {
	writeText(writer, http.StatusOK, "ok\n")
}

func (a *adminServer) serveReady(writer http.ResponseWriter, _ *http.Request) {
	health := Health{Ready: true}
	if a.health != nil {
		health = a.health()
	}
	if !health.Ready {
		writeText(writer, http.StatusServiceUnavailable, "not ready: "+safeReason(health.Reason)+"\n")
		return
	}
	writeText(writer, http.StatusOK, "ready\n")
}

// serveMetrics writes the Prometheus text exposition format by hand.
//
// By hand, rather than through a client library, because the whole surface is a
// handful of unlabelled counters and a dependency that can create time series
// is a dependency that can create unbounded ones.
func (a *adminServer) serveMetrics(writer http.ResponseWriter, _ *http.Request) {
	var exported []metric
	if a.metrics != nil {
		exported = a.metrics()
	}
	sort.Slice(exported, func(i, j int) bool { return exported[i].name < exported[j].name })
	var body strings.Builder
	for _, m := range exported {
		body.WriteString("# HELP " + m.name + " " + m.help + "\n")
		body.WriteString("# TYPE " + m.name + " counter\n")
		body.WriteString(m.name + " " + strconv.FormatUint(m.value, 10) + "\n")
	}
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(body.String()))
}

func writeText(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(body))
}

// safeReason keeps a readiness reason to a short categorical token. This body
// is read by whoever can reach the admin port, so a reason assembled from
// anything record-derived must not be able to reach it.
func safeReason(reason string) string {
	const maximum = 64
	if reason == "" || len(reason) > maximum {
		return "unavailable"
	}
	for _, r := range reason {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == ' ':
		default:
			return "unavailable"
		}
	}
	return reason
}
