/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

// Package sshserver_test exercises the SSH server through the WebSocket proxy,
// which is the path a desktop IDE takes. It uses no cluster and no external
// binaries: the WebSocket connection is adapted to a net.Conn and handed to an
// SSH client directly, the same way a ProxyCommand would.
package sshserver_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/gorilla/websocket"
	"github.com/pkg/sftp"
	"go.uber.org/zap"
	gossh "golang.org/x/crypto/ssh"

	"github.com/jupyter-infra/workspace-websocket-proxy/internal/proxy"
	"github.com/jupyter-infra/workspace-websocket-proxy/internal/sshserver"
)

const (
	startupTimeout = 5 * time.Second
	dialTimeout    = 10 * time.Second
)

func testLogger() logr.Logger {
	zapLog, _ := zap.NewDevelopment()
	return zapr.NewLogger(zapLog)
}

// freeLoopbackPort reserves and releases a loopback port so a server can bind it.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve a port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	return listener.Addr().(*net.TCPAddr).Port
}

// waitForListener blocks until addr accepts a connection.
func waitForListener(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s after %s", addr, startupTimeout)
}

// startSSHServer runs the SSH server on a free loopback port and returns its address.
func startSSHServer(t *testing.T) string {
	t.Helper()

	port := freeLoopbackPort(t)
	config := &sshserver.Config{
		ListenAddr:  fmt.Sprintf("127.0.0.1:%d", port),
		HostKeyPath: filepath.Join(t.TempDir(), "host_key"),
		MaxSessions: 5,
	}

	server, err := sshserver.New(config, testLogger())
	if err != nil {
		t.Fatalf("failed to create SSH server: %v", err)
	}

	go func() { _ = server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	waitForListener(t, config.ListenAddr)
	return config.ListenAddr
}

// startProxy runs the WebSocket proxy in front of target and returns its address.
func startProxy(t *testing.T, target string) string {
	t.Helper()

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatalf("invalid target %q: %v", target, err)
	}
	targetPort, err := net.LookupPort("tcp", portStr)
	if err != nil {
		t.Fatalf("invalid target port %q: %v", portStr, err)
	}

	listenPort := freeLoopbackPort(t)
	config := &proxy.Config{
		ListenAddr:         fmt.Sprintf("127.0.0.1:%d", listenPort),
		TargetHost:         host,
		TargetPort:         targetPort,
		MaxSessionDuration: time.Minute,
		PingInterval:       time.Second,
		PingTimeout:        2 * time.Second,
		MaxConnections:     5,
		ReadLimit:          65536,
	}

	server := proxy.NewServer(config, testLogger())
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return
		}
	}()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	waitForListener(t, config.ListenAddr)
	return config.ListenAddr
}

// wsConn adapts a WebSocket connection to net.Conn so an SSH client can use it as
// its transport. Only binary frames carry payload, matching what the proxy bridges.
type wsConn struct {
	ws      *websocket.Conn
	pending io.Reader
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.pending != nil {
			n, err := c.pending.Read(p)
			if errors.Is(err, io.EOF) {
				c.pending = nil
				if n > 0 {
					return n, nil
				}
				continue
			}
			return n, err
		}

		messageType, reader, err := c.ws.NextReader()
		if err != nil {
			return 0, err
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		c.pending = reader
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error                      { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr               { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr              { return c.ws.RemoteAddr() }
func (c *wsConn) SetReadDeadline(t time.Time) error { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error {
	return c.ws.SetWriteDeadline(t)
}

func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// dialSSHThroughProxy opens an SSH client whose transport is a WebSocket
// connection to the proxy.
func dialSSHThroughProxy(t *testing.T, proxyAddr string) *gossh.Client {
	t.Helper()

	dialer := websocket.Dialer{HandshakeTimeout: dialTimeout}
	ws, _, err := dialer.Dial(fmt.Sprintf("ws://%s/", proxyAddr), nil)
	if err != nil {
		t.Fatalf("failed to dial the proxy: %v", err)
	}

	transport := &wsConn{ws: ws}
	clientConfig := &gossh.ClientConfig{
		User:            "workspace",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         dialTimeout,
	}

	conn, chans, reqs, err := gossh.NewClientConn(transport, "workspace", clientConfig)
	if err != nil {
		_ = transport.Close()
		t.Fatalf("failed to establish the SSH connection: %v", err)
	}

	client := gossh.NewClient(conn, chans, reqs)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func newSessionStack(t *testing.T) *gossh.Client {
	t.Helper()
	return dialSSHThroughProxy(t, startProxy(t, startSSHServer(t)))
}

func TestSSHOverWebSocketRunsCommand(t *testing.T) {
	client := newSessionStack(t)

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("failed to open a session: %v", err)
	}
	defer func() { _ = session.Close() }()

	output, err := session.Output("echo integration-ok")
	if err != nil {
		t.Fatalf("failed to run the command: %v", err)
	}

	if got := string(output); got != "integration-ok\n" {
		t.Errorf("expected %q, got %q", "integration-ok\n", got)
	}
}

func TestSSHOverWebSocketPropagatesExitCode(t *testing.T) {
	client := newSessionStack(t)

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("failed to open a session: %v", err)
	}
	defer func() { _ = session.Close() }()

	err = session.Run("exit 33")

	var exitErr *gossh.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an *ssh.ExitError, got %v", err)
	}
	if exitErr.ExitStatus() != 33 {
		t.Errorf("expected exit status 33, got %d", exitErr.ExitStatus())
	}
}

func TestSSHOverWebSocketPassesSessionEnvironment(t *testing.T) {
	client := newSessionStack(t)

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("failed to open a session: %v", err)
	}
	defer func() { _ = session.Close() }()

	if err := session.Setenv("INTEGRATION_KEY", "integration-value"); err != nil {
		t.Skipf("server declined the environment request: %v", err)
	}

	output, err := session.Output("printf %s \"$INTEGRATION_KEY\"")
	if err != nil {
		t.Fatalf("failed to run the command: %v", err)
	}

	if got := string(output); got != "integration-value" {
		t.Errorf("expected %q, got %q", "integration-value", got)
	}
}

func TestSSHOverWebSocketAllocatesPty(t *testing.T) {
	client := newSessionStack(t)

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("failed to open a session: %v", err)
	}
	defer func() { _ = session.Close() }()

	modes := gossh.TerminalModes{gossh.ECHO: 0}
	if err := session.RequestPty("xterm", 24, 80, modes); err != nil {
		t.Fatalf("failed to request a pty: %v", err)
	}

	output, err := session.Output("tty")
	if err != nil {
		t.Fatalf("failed to run the command: %v", err)
	}

	if len(output) == 0 {
		t.Error("expected tty to report a terminal device")
	}
}

func TestSFTPOverWebSocketRoundTrip(t *testing.T) {
	client := newSessionStack(t)

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("failed to open an SFTP client: %v", err)
	}
	defer func() { _ = sftpClient.Close() }()

	path := filepath.Join(t.TempDir(), "payload.txt")
	want := []byte("sftp round trip")

	remoteFile, err := sftpClient.Create(path)
	if err != nil {
		t.Fatalf("failed to create the remote file: %v", err)
	}
	if _, err := remoteFile.Write(want); err != nil {
		t.Fatalf("failed to write the remote file: %v", err)
	}
	if err := remoteFile.Close(); err != nil {
		t.Fatalf("failed to close the remote file: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the file back: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestLocalPortForwardReachesLoopbackService(t *testing.T) {
	backend := newEchoBackend(t)
	client := newSessionStack(t)

	conn, err := client.Dial("tcp", backend)
	if err != nil {
		t.Fatalf("failed to open a forwarded connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("forwarded")); err != nil {
		t.Fatalf("failed to write to the forwarded connection: %v", err)
	}

	echoed, err := readFullWithin(conn, len("forwarded"), dialTimeout)
	if err != nil {
		t.Fatalf("failed to read from the forwarded connection: %v", err)
	}

	if string(echoed) != "forwarded" {
		t.Errorf("expected %q, got %q", "forwarded", echoed)
	}
}

// readFullWithin reads exactly n bytes, bounding the wait. SSH channels do not
// support read deadlines, so the timeout is enforced by the caller.
func readFullWithin(reader io.Reader, n int, timeout time.Duration) ([]byte, error) {
	type result struct {
		buf []byte
		err error
	}

	done := make(chan result, 1)
	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(reader, buf)
		done <- result{buf: buf, err: err}
	}()

	select {
	case r := <-done:
		return r.buf, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("read of %d bytes did not complete within %s", n, timeout)
	}
}

func TestLocalPortForwardRejectsNonLoopbackDestination(t *testing.T) {
	client := newSessionStack(t)

	if conn, err := client.Dial("tcp", "10.0.0.5:80"); err == nil {
		_ = conn.Close()
		t.Error("expected a forward to a routable address to be refused")
	}
}

func TestProxyTargetHealthReflectsSSHServer(t *testing.T) {
	proxyAddr := startProxy(t, startSSHServer(t))

	resp, err := http.Get(fmt.Sprintf("http://%s/health/target", proxyAddr))
	if err != nil {
		t.Fatalf("failed to request target health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200 with the SSH server running, got %d", resp.StatusCode)
	}
}

// newEchoBackend starts a loopback TCP echo server and returns its address.
func newEchoBackend(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start the echo backend: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	return listener.Addr().String()
}
