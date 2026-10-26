package connector

import (
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/elsbrock/mautrix-chatto/pkg/bridgemetrics"
)

// startMetrics serves /metrics, /healthz and /readyz on the configured address.
func (c *Connector) startMetrics() error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(bridgemetrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", c.serveReady)
	ln, err := net.Listen("tcp", c.Config.Metrics.Listen)
	if err != nil {
		return err
	}
	c.metricsServer = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := c.metricsServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.Bridge.Log.Err(err).Msg("Metrics server failed")
		}
	}()
	c.Bridge.Log.Info().Str("listen", ln.Addr().String()).Msg("Serving metrics and health endpoints")
	return nil
}

// serveReady reports ready once the connector started and the Matrix side is
// up. It does not depend on individual logins being connected.
func (c *Connector) serveReady(w http.ResponseWriter, _ *http.Request) {
	if !c.isReady() {
		http.Error(w, "starting\n", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}
