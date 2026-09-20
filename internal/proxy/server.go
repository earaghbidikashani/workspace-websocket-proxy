/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	healthPath                = "/health"
	targetHealthPath          = "/health/target"
	metricsPath               = "/metrics"
	targetHealthDialTimeout   = 2 * time.Second
	targetBannerReadLimit     = 255
	capacityRetryAfterSeconds = "5"
)

// Server is the main WebSocket proxy HTTP server.
type Server struct {
	config         *Config
	metrics        *Metrics
	sessionManager *SessionManager
	revalidator    Revalidator
	logger         logr.Logger
	httpServer     *http.Server

	targetHealth *targetHealth

	proberCtx    context.Context
	proberCancel context.CancelFunc

	proberMu      sync.Mutex
	proberDone    chan struct{}
	proberStopped bool
}

// NewServer creates a new proxy Server.
func NewServer(config *Config, logger logr.Logger) *Server {
	s := &Server{
		config:         config,
		metrics:        NewMetrics(),
		sessionManager: NewSessionManager(config.MaxConnections),
		revalidator:    &NoOpRevalidator{},
		logger:         logger.WithName("server"),
		targetHealth:   &targetHealth{},
	}
	s.proberCtx, s.proberCancel = context.WithCancel(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, s.handleHealth)
	mux.HandleFunc(targetHealthPath, s.handleTargetHealth)
	mux.Handle(metricsPath, promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/", s.handleWebSocket)

	s.httpServer = &http.Server{
		Addr:    config.ListenAddr,
		Handler: mux,
	}

	return s
}

// ListenAndServe starts the target prober and the HTTP server.
func (s *Server) ListenAndServe() error {
	s.logger.Info("Starting WebSocket proxy",
		"addr", s.config.ListenAddr,
		"target", s.config.TargetAddr(),
		"maxConnections", s.config.MaxConnections,
		"maxSessionDuration", s.config.MaxSessionDuration,
		"pingInterval", s.config.PingInterval,
		"pingTimeout", s.config.PingTimeout,
		"targetHealthInterval", s.config.TargetHealthInterval,
	)

	s.startTargetProber()
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the server and the target prober.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopTargetProber()
	return s.httpServer.Shutdown(ctx)
}

// handleHealth responds with 200 OK for Kubernetes probes.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status":"ok","activeConnections":%d}`, s.sessionManager.ActiveCount())
}

// handleTargetHealth reports the last probe result. For alerting, not readiness.
func (s *Server) handleTargetHealth(w http.ResponseWriter, _ *http.Request) {
	target := s.config.TargetAddr()
	result := s.targetHealth.snapshot()

	w.Header().Set("Content-Type", "application/json")

	switch {
	case !result.checked:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"status":"unknown","target":%q}`, target)
	case result.reachable:
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"ok","target":%q,"checkedAt":%q}`,
			target, result.at.UTC().Format(time.RFC3339))
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"status":"unreachable","target":%q,"checkedAt":%q,"error":%q}`,
			target, result.at.UTC().Format(time.RFC3339), result.err)
	}
}

// probeTarget dials addr and, when bannerPrefix is set, checks the greeting.
func probeTarget(ctx context.Context, addr, bannerPrefix string) error {
	conn, err := dialTCP(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if bannerPrefix == "" {
		return nil
	}

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return fmt.Errorf("failed to set read deadline: %w", err)
		}
	}

	banner, err := bufio.NewReader(io.LimitReader(conn, targetBannerReadLimit)).ReadString('\n')
	if err != nil && banner == "" {
		return fmt.Errorf("connected but read no greeting: %w", err)
	}
	if !strings.HasPrefix(banner, bannerPrefix) {
		return fmt.Errorf("greeting %q does not start with %q", strings.TrimRight(banner, "\r\n"), bannerPrefix)
	}
	return nil
}

// upgrader configures the WebSocket upgrade.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Origin check is handled by Traefik ForwardAuth before traffic reaches us.
	CheckOrigin: func(_ *http.Request) bool { return true },
}

// handleWebSocket upgrades the HTTP connection and starts a proxied session.
// Precondition: authentication is handled externally by Traefik ForwardAuth
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	logger := s.logger.WithValues("remoteAddr", r.RemoteAddr)

	if !s.sessionManager.Acquire() {
		logger.Info("Connection rejected: at capacity",
			"active", s.sessionManager.ActiveCount(),
			"max", s.config.MaxConnections)
		s.metrics.ConnectionErrors.WithLabelValues("capacity_exceeded").Inc()
		w.Header().Set("Retry-After", capacityRetryAfterSeconds)
		http.Error(w, "Too many concurrent connections", http.StatusTooManyRequests)
		return
	}
	defer s.sessionManager.Release()

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error(err, "WebSocket upgrade failed")
		s.metrics.ConnectionErrors.WithLabelValues("upgrade_failed").Inc()
		return
	}
	defer ws.Close()

	s.metrics.ConnectionsTotal.Inc()
	s.metrics.ActiveConnections.Inc()
	defer s.metrics.ActiveConnections.Dec()

	logger.Info("WebSocket connection established")

	session := NewSession(ws, s.config, s.metrics, logger, s.revalidator)
	if err := session.Run(r.Context()); err != nil {
		if err != context.Canceled {
			logger.V(1).Info("Session ended with error", "error", err)
		}
	}

	logger.Info("WebSocket connection closed")
}

// dialTCP establishes a TCP connection to the target with timeout.
func dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
	}
	return dialer.DialContext(ctx, "tcp", addr)
}
