/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHealthEndpoint(t *testing.T) {
	config := testConfig()
	server := NewServer(config, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("expected body to contain 'ok', got %s", w.Body.String())
	}
}

func TestHealthEndpointDoesNotDialTarget(t *testing.T) {
	config := testConfig()
	config.TargetHost = "127.0.0.1"
	config.TargetPort = 1

	server := NewServer(config, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected /health to stay 200 with an unreachable target, got %d", w.Code)
	}
}

func TestTargetHealthEndpointReachable(t *testing.T) {
	addr, cleanup := startEchoTCPServer(t)
	defer cleanup()

	code, body, server := targetHealthStatus(t, addr, "")

	if code != http.StatusOK {
		t.Errorf("expected status 200, got %d", code)
	}
	if !strings.Contains(body, "ok") {
		t.Errorf("expected body to contain 'ok', got %s", body)
	}

	assertMetricPresent(t, server, "ws_proxy_target_reachable 1")
}

func TestTargetHealthEndpointUnreachable(t *testing.T) {
	code, body, server := targetHealthStatus(t, "127.0.0.1:1", "")

	if code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", code)
	}
	if !strings.Contains(body, "unreachable") {
		t.Errorf("expected body to contain 'unreachable', got %s", body)
	}

	assertMetricPresent(t, server, "ws_proxy_target_reachable 0")
}

// targetHealthStatus runs one /health/target request against a server pointed at
// addr with the given expected banner prefix.
func targetHealthStatus(t *testing.T, addr, bannerPrefix string) (int, string, *Server) {
	t.Helper()

	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	config := testConfig()
	config.TargetHost = "127.0.0.1"
	config.TargetPort = port
	config.TargetHealthBannerPrefix = bannerPrefix

	server := NewServer(config, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), targetHealthDialTimeout)
	defer cancel()
	server.probeOnce(ctx)

	req := httptest.NewRequest(http.MethodGet, "/health/target", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	return w.Code, w.Body.String(), server
}

func TestTargetHealthAcceptsExpectedBanner(t *testing.T) {
	addr, cleanup := startBannerTCPServer(t, "SSH-2.0-remote-access-server\r\n")
	defer cleanup()

	code, body, server := targetHealthStatus(t, addr, defaultTargetHealthBannerPrefix)

	if code != http.StatusOK {
		t.Errorf("expected status 200, got %d (body %s)", code, body)
	}
	assertMetricPresent(t, server, "ws_proxy_target_reachable 1")
}

func TestTargetHealthRejectsWrongBanner(t *testing.T) {
	addr, cleanup := startBannerTCPServer(t, "HTTP/1.1 200 OK\r\n")
	defer cleanup()

	code, body, server := targetHealthStatus(t, addr, defaultTargetHealthBannerPrefix)

	if code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503 for a non-SSH greeting, got %d", code)
	}
	if !strings.Contains(body, "does not start with") {
		t.Errorf("expected body to explain the greeting mismatch, got %s", body)
	}
	assertMetricPresent(t, server, "ws_proxy_target_reachable 0")
}

// A bare dial cannot tell a wedged target from a healthy one, which is the whole
// reason the banner check exists.
func TestTargetHealthDetectsWedgedTarget(t *testing.T) {
	addr, cleanup := startWedgedTCPServer(t)
	defer cleanup()

	code, _, _ := targetHealthStatus(t, addr, "")
	if code != http.StatusOK {
		t.Fatalf("precondition: a bare dial should succeed off the listen backlog, got %d", code)
	}

	code, body, server := targetHealthStatus(t, addr, defaultTargetHealthBannerPrefix)
	if code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503 for a listening but wedged target, got %d", code)
	}
	if !strings.Contains(body, "read no greeting") {
		t.Errorf("expected body to report a missing greeting, got %s", body)
	}
	assertMetricPresent(t, server, "ws_proxy_target_reachable 0")
}

func TestTargetHealthSkipsBannerWhenPrefixEmpty(t *testing.T) {
	addr, cleanup := startEchoTCPServer(t)
	defer cleanup()

	code, body, _ := targetHealthStatus(t, addr, "")
	if code != http.StatusOK {
		t.Errorf("expected status 200 with the banner check disabled, got %d (body %s)", code, body)
	}
}

func TestTargetHealthReportsUnknownBeforeTheFirstProbe(t *testing.T) {
	server := NewServer(testConfig(), testLogger())

	req := httptest.NewRequest(http.MethodGet, "/health/target", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503 before any probe, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unknown") {
		t.Errorf("expected an unmeasured target to report unknown, got %s", w.Body.String())
	}
}

func TestTargetHealthDoesNotDialWhenAsked(t *testing.T) {
	addr, cleanup := startCountingTCPServer(t)
	defer cleanup()

	server := newServerForTarget(t, addr, "")

	ctx, cancel := context.WithTimeout(context.Background(), targetHealthDialTimeout)
	defer cancel()
	server.probeOnce(ctx)

	waitForConnectionCount(t, addr, 1)
	before := connectionCount(addr)

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/health/target", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected the cached result to be served, got %d", w.Code)
		}
	}

	if after := connectionCount(addr); after != before {
		t.Errorf("requests opened %d connections to the target; the endpoint must report, not probe",
			after-before)
	}
}

func TestTargetProberSetsTheMetricWithoutAnyRequest(t *testing.T) {
	addr, cleanup := startBannerTCPServer(t, "SSH-2.0-test\r\n")
	defer cleanup()

	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	config := testConfig()
	config.TargetPort = port
	config.TargetHealthBannerPrefix = defaultTargetHealthBannerPrefix

	server := NewServer(config, testLogger())
	server.startTargetProber()
	defer server.stopTargetProber()

	waitForMetric(t, server, "ws_proxy_target_reachable 1")
}

func TestTargetProberReportsAnUnreachableTarget(t *testing.T) {
	config := testConfig()
	config.TargetPort = 1

	server := NewServer(config, testLogger())
	server.startTargetProber()
	defer server.stopTargetProber()

	body := waitForTargetStatus(t, server, "unreachable")
	if !strings.Contains(body, "checkedAt") {
		t.Errorf("expected the response to report when it was measured, got %s", body)
	}
	assertMetricPresent(t, server, "ws_proxy_target_reachable 0")
}

func TestTargetProberStopsOnShutdown(t *testing.T) {
	addr, cleanup := startCountingTCPServer(t)
	defer cleanup()

	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	config := testConfig()
	config.TargetPort = port

	server := NewServer(config, testLogger())
	server.startTargetProber()

	waitForMetric(t, server, "ws_proxy_target_reachable 1")
	server.stopTargetProber()

	settled := connectionCount(addr)
	time.Sleep(4 * config.TargetHealthInterval)

	if after := connectionCount(addr); after != settled {
		t.Errorf("the prober kept running after shutdown: %d further connections", after-settled)
	}
}

func TestStopTargetProberIsSafeWithoutStart(t *testing.T) {
	NewServer(testConfig(), testLogger()).stopTargetProber()
}

func TestListenAndServeThenShutdownOnAnotherGoroutine(t *testing.T) {
	config := testConfig()
	config.ListenAddr = "127.0.0.1:0"

	server := NewServer(config, testLogger())

	started := make(chan struct{})
	go func() {
		close(started)
		_ = server.ListenAndServe()
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		t.Errorf("expected a clean shutdown, got %v", err)
	}
}

func TestShutdownBeforeListenAndServeStillStopsTheProber(t *testing.T) {
	config := testConfig()
	config.ListenAddr = "127.0.0.1:0"

	server := NewServer(config, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	server.startTargetProber()

	server.proberMu.Lock()
	defer server.proberMu.Unlock()
	if server.proberDone != nil {
		t.Error("expected starting after shutdown to be a no-op")
	}
}

func TestTargetProberToleratesNonPositiveInterval(t *testing.T) {
	config := testConfig()
	config.TargetHealthInterval = 0

	server := NewServer(config, testLogger())
	server.startTargetProber()
	server.stopTargetProber()
}

func newServerForTarget(t *testing.T, addr, bannerPrefix string) *Server {
	t.Helper()

	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	config := testConfig()
	config.TargetHost = "127.0.0.1"
	config.TargetPort = port
	config.TargetHealthBannerPrefix = bannerPrefix

	return NewServer(config, testLogger())
}

func waitForConnectionCount(t *testing.T, addr string, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if connectionCount(addr) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %d connections to %s, saw %d", want, addr, connectionCount(addr))
}

func waitForTargetStatus(t *testing.T, server *Server, want string) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/health/target", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if strings.Contains(w.Body.String(), want) {
			return w.Body.String()
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for /health/target to report %q", want)
	return ""
}

func waitForMetric(t *testing.T, server *Server, want string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if strings.Contains(w.Body.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for metrics to contain %q", want)
}

func TestCapacityRejectionSetsRetryAfter(t *testing.T) {
	config := testConfig()
	config.MaxConnections = 1
	server := NewServer(config, testLogger())

	if !server.sessionManager.Acquire() {
		t.Fatal("expected to acquire the only slot")
	}
	defer server.sessionManager.Release()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != capacityRetryAfterSeconds {
		t.Errorf("expected Retry-After %q, got %q", capacityRetryAfterSeconds, got)
	}
}

func assertMetricPresent(t *testing.T, server *Server, want string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("expected metrics to contain %q", want)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	config := testConfig()
	server := NewServer(config, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ws_proxy") {
		t.Errorf("expected metrics output to contain ws_proxy prefix")
	}
}

func TestMaxConnections(t *testing.T) {
	tcpAddr, cleanupTCP := startEchoTCPServer(t)
	defer cleanupTCP()

	_, portStr, _ := net.SplitHostPort(tcpAddr)
	port, _ := strconv.Atoi(portStr)

	config := testConfig()
	config.TargetHost = "127.0.0.1"
	config.TargetPort = port
	config.MaxConnections = 1

	server := NewServer(config, testLogger())
	ts := httptest.NewServer(server.httpServer.Handler)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	ws1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws1.Close()

	time.Sleep(50 * time.Millisecond)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected second connection to be rejected")
	}
	if resp != nil && resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", resp.StatusCode)
	}
}

func TestTCPDialFailure(t *testing.T) {
	config := testConfig()
	config.TargetHost = "127.0.0.1"
	config.TargetPort = 1

	server := NewServer(config, testLogger())
	ts := httptest.NewServer(server.httpServer.Handler)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))

	_, _, err = ws.ReadMessage()
	if err == nil {
		t.Error("expected read to fail after TCP dial failure")
	}
}
