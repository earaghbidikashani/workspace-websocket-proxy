/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

// Package sshserver provides the SSH server that terminates remote IDE sessions
// inside the workspace container.
//
// Two properties are load-bearing and easy to "fix" wrongly.
//
// The server runs in the workspace container, not a sidecar. An SSH server spawns
// the login shell as its own child process, so the shell inherits the server's
// mount namespace. Running it anywhere else gives the user a shell with none of
// their files, interpreter, or kernels.
//
// The server performs no SSH authentication and listens on loopback only.
// Authentication happens at the ingress, which validates a short-lived JWT before
// any byte reaches the tunnel; loopback binding is what keeps the socket
// unreachable from outside the pod. Config.Validate refuses a non-loopback bind
// for this reason.
package sshserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/go-logr/logr"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

const (
	// exitCodeGeneralError is reported when the server itself fails to run the
	// requested command.
	exitCodeGeneralError = 1

	// exitCodeCommandNotExecutable mirrors the shell convention for a command that
	// could not be started.
	exitCodeCommandNotExecutable = 127

	// signalExitCodeBase mirrors the shell convention of reporting a signalled
	// child as 128 plus the signal number.
	signalExitCodeBase = 128

	// hostKeyDirMode and hostKeyFileMode keep the private key readable only by its
	// owner.
	hostKeyDirMode  = 0o700
	hostKeyFileMode = 0o600

	// shellBash and shellSh are the fallback shells, tried in order when SHELL is
	// unset or names a path that does not exist.
	shellBash = "/bin/bash"
	shellSh   = "/bin/sh"
)

// Server wraps a gliderlabs SSH server with the handlers a desktop IDE needs.
type Server struct {
	config   *Config
	logger   logr.Logger
	ssh      *ssh.Server
	sessions atomic.Int64
}

// New validates the configuration, loads or generates the host key, and builds
// the SSH server.
func New(config *Config, logger logr.Logger) (*Server, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	signer, generated, err := loadOrCreateHostKey(config.HostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare host key: %w", err)
	}

	logger.Info("Host key ready",
		"path", config.HostKeyPath,
		"generated", generated,
		"fingerprint", gossh.FingerprintSHA256(signer.PublicKey()))

	if config.HasEphemeralHostKeyPath() {
		logger.Info("Host key path is on ephemeral storage and will not survive a container restart",
			"path", config.HostKeyPath)
	}

	s := &Server{config: config, logger: logger}
	forwardHandler := &ssh.ForwardedTCPHandler{}

	s.ssh = &ssh.Server{
		Addr:                          config.ListenAddr,
		HostSigners:                   []ssh.Signer{signer},
		IdleTimeout:                   config.IdleTimeout,
		Handler:                       s.handleSession,
		LocalPortForwardingCallback:   s.allowLocalForward,
		ReversePortForwardingCallback: s.allowReverseForward,

		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      ssh.DefaultSessionHandler,
			"direct-tcpip": ssh.DirectTCPIPHandler,
		},
		RequestHandlers: map[string]ssh.RequestHandler{
			"tcpip-forward":        forwardHandler.HandleSSHRequest,
			"cancel-tcpip-forward": forwardHandler.HandleSSHRequest,
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": s.handleSFTP,
		},
	}

	return s, nil
}

// ListenAndServe starts the SSH server and blocks until it stops.
func (s *Server) ListenAndServe() error {
	s.logger.Info("Starting SSH server",
		"addr", s.config.ListenAddr,
		"maxSessions", s.config.MaxSessions,
		"idleTimeout", s.config.IdleTimeout,
		"loginShell", s.config.LoginShell,
		"authentication", "none (loopback only, authenticated at the ingress)")
	return s.ssh.ListenAndServe()
}

// Shutdown stops accepting new connections.
func (s *Server) Shutdown(_ context.Context) error {
	return s.ssh.Close()
}

// allowLocalForward permits forwarding only to loopback destinations inside the
// pod. An IDE uses this to reach services in the workspace, such as the Jupyter
// port or a dev server; the pod's network namespace is the intended blast radius,
// not the cluster network.
func (s *Server) allowLocalForward(_ ssh.Context, host string, port uint32) bool {
	if !isLoopback(host) {
		s.logger.Info("Rejected local forward to non-loopback destination", "host", host, "port", port)
		return false
	}
	s.logger.V(1).Info("Accepted local forward", "host", host, "port", port)
	return true
}

// allowReverseForward permits remote forwarding only onto loopback bind
// addresses. gliderlabs rejects every reverse forward when this callback is
// absent, so it must be set for remote port forwarding to work at all; binding a
// routable address would expose the forwarded port to the cluster network.
func (s *Server) allowReverseForward(_ ssh.Context, bindHost string, bindPort uint32) bool {
	if bindHost == "" {
		s.logger.V(1).Info("Accepted reverse forward on implicit loopback bind", "port", bindPort)
		return true
	}
	if !isLoopback(bindHost) {
		s.logger.Info("Rejected reverse forward on non-loopback bind address",
			"bindHost", bindHost, "bindPort", bindPort)
		return false
	}
	s.logger.V(1).Info("Accepted reverse forward", "bindHost", bindHost, "bindPort", bindPort)
	return true
}

// handleSession runs a command or an interactive shell for the session.
func (s *Server) handleSession(session ssh.Session) {
	if !s.acquireSession() {
		s.logger.Info("Rejecting session: at capacity", "maxSessions", s.config.MaxSessions)
		_, _ = fmt.Fprintln(session.Stderr(), "remote access server at capacity")
		_ = session.Exit(exitCodeGeneralError)
		return
	}
	defer s.releaseSession()

	logger := s.logger.WithValues(
		"remoteAddr", session.RemoteAddr().String(),
		"user", session.User())
	logger.Info("Session opened")

	cmd := buildShellCommand(resolveShell(), s.config.LoginShell, session.RawCommand())
	cmd.Env = mergeEnv(session.Environ())
	if home, ok := lookupEnv(cmd.Env, "HOME"); ok {
		cmd.Dir = home
	}

	ptyReq, winCh, isPty := session.Pty()
	if isPty {
		s.runWithPty(session, cmd, ptyReq, winCh, logger)
		return
	}
	s.runWithoutPty(session, cmd, logger)
}

// acquireSession reserves a session slot, reporting false when the server is at
// capacity. A zero MaxSessions disables the cap.
func (s *Server) acquireSession() bool {
	if s.config.MaxSessions <= 0 {
		return true
	}
	if s.sessions.Add(1) > int64(s.config.MaxSessions) {
		s.sessions.Add(-1)
		return false
	}
	return true
}

// releaseSession returns a session slot reserved by acquireSession.
func (s *Server) releaseSession() {
	if s.config.MaxSessions <= 0 {
		return
	}
	s.sessions.Add(-1)
}

// runWithPty attaches the command to a pseudo-terminal, which is what an
// interactive IDE terminal needs.
func (s *Server) runWithPty(
	session ssh.Session,
	cmd *exec.Cmd,
	ptyReq ssh.Pty,
	winCh <-chan ssh.Window,
	logger logr.Logger,
) {
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("TERM=%s", ptyReq.Term),
		"COLORTERM=truecolor")

	f, err := pty.Start(cmd)
	if err != nil {
		logger.Error(err, "Failed to start PTY")
		_, _ = fmt.Fprintf(session.Stderr(), "failed to allocate pty: %v\n", err)
		_ = session.Exit(exitCodeGeneralError)
		return
	}
	setWinsize(f, ptyReq.Window.Width, ptyReq.Window.Height)
	stopResize, resizeDone := s.watchWindowSize(f, winCh)

	go func() {
		_, _ = io.Copy(f, session)
	}()
	_, _ = io.Copy(session, f)

	close(stopResize)
	<-resizeDone
	_ = f.Close()

	s.finish(session, cmd, logger)
}

// watchWindowSize propagates client resize events to the pty until the returned
// stop channel is closed. The caller must wait on the returned done channel
// before closing the pty: resizing reads the raw file descriptor, which is
// invalid once the file is closed.
func (s *Server) watchWindowSize(f *os.File, winCh <-chan ssh.Window) (stop chan struct{}, done chan struct{}) {
	stop = make(chan struct{})
	done = make(chan struct{})

	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case win, ok := <-winCh:
				if !ok {
					return
				}
				setWinsize(f, win.Width, win.Height)
			}
		}
	}()

	return stop, done
}

// runWithoutPty wires stdio directly. Remote-SSH uses this path for its bootstrap
// commands, so the exit status has to be faithful.
func (s *Server) runWithoutPty(session ssh.Session, cmd *exec.Cmd, logger logr.Logger) {
	cmd.Stdout = session
	cmd.Stderr = session.Stderr()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		logger.Error(err, "Failed to create stdin pipe")
		_ = session.Exit(exitCodeGeneralError)
		return
	}

	if err := cmd.Start(); err != nil {
		logger.Error(err, "Failed to start command")
		_, _ = fmt.Fprintf(session.Stderr(), "failed to start command: %v\n", err)
		_ = session.Exit(exitCodeCommandNotExecutable)
		return
	}

	go func() {
		_, _ = io.Copy(stdin, session)
		_ = stdin.Close()
	}()

	s.finish(session, cmd, logger)
}

// finish waits for the child and propagates its real exit status. Remote-SSH
// branches on these codes during bootstrap, so swallowing them breaks the
// connection in ways that are hard to diagnose from the client side.
func (s *Server) finish(session ssh.Session, cmd *exec.Cmd, logger logr.Logger) {
	code := exitCode(cmd.Wait())
	logger.Info("Session closed", "exitCode", code)
	_ = session.Exit(code)
}

// exitCode maps the result of exec.Cmd.Wait to an SSH exit status, reporting a
// signalled child as 128 plus the signal number.
func exitCode(err error) int {
	if err == nil {
		return 0
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return exitCodeGeneralError
	}

	if code := exitErr.ExitCode(); code >= 0 {
		return code
	}

	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return signalExitCodeBase + int(status.Signal())
	}
	return exitCodeGeneralError
}

// handleSFTP serves the sftp subsystem, which IDEs use to transfer files and to
// install their remote server component.
func (s *Server) handleSFTP(session ssh.Session) {
	logger := s.logger.WithValues("remoteAddr", session.RemoteAddr().String())
	logger.Info("SFTP session opened")

	server, err := sftp.NewServer(session, sftp.WithDebug(io.Discard))
	if err != nil {
		logger.Error(err, "Failed to start SFTP server")
		_ = session.Exit(exitCodeGeneralError)
		return
	}
	defer func() { _ = server.Close() }()

	if err := server.Serve(); err != nil && !errors.Is(err, io.EOF) {
		logger.Error(err, "SFTP server exited with error")
		_ = session.Exit(exitCodeGeneralError)
		return
	}

	logger.Info("SFTP session closed")
	_ = session.Exit(0)
}

// buildShellCommand assembles the child process for a session. A non-empty
// rawCmd runs through the shell so that quoting and redirection behave as a
// client expects.
func buildShellCommand(shell string, loginShell bool, rawCmd string) *exec.Cmd {
	if strings.TrimSpace(rawCmd) == "" {
		if loginShell {
			return exec.Command(shell, "-l")
		}
		return exec.Command(shell)
	}

	if loginShell {
		return exec.Command(shell, "-lc", rawCmd)
	}
	return exec.Command(shell, "-c", rawCmd)
}

// resolveShell picks the user's shell, preferring SHELL and falling back to
// whatever exists. A missing shell would otherwise fail every session with an
// opaque error.
func resolveShell() string {
	candidates := []string{os.Getenv("SHELL"), shellBash, shellSh}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return shellSh
}

// mergeEnv overlays the client-supplied environment on the process environment.
// The client's values win, matching sshd with AcceptEnv.
func mergeEnv(sessionEnv []string) []string {
	merged := map[string]string{}
	for _, entry := range append(os.Environ(), sessionEnv...) {
		if key, value, ok := strings.Cut(entry, "="); ok {
			merged[key] = value
		}
	}

	out := make([]string, 0, len(merged))
	for key, value := range merged {
		out = append(out, key+"="+value)
	}
	return out
}

// lookupEnv returns the value of key in an environment slice.
func lookupEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

// loadOrCreateHostKey returns the persisted host key, generating one on first
// use. Reusing a key across restarts is what stops clients from reporting a
// changed host key every time the process comes back.
func loadOrCreateHostKey(path string) (signer ssh.Signer, generated bool, err error) {
	pemBytes, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		parsed, parseErr := gossh.ParsePrivateKey(pemBytes)
		if parseErr != nil {
			return nil, false, fmt.Errorf("existing host key at %s is unreadable: %w", path, parseErr)
		}
		return parsed, false, nil
	case !errors.Is(readErr, os.ErrNotExist):
		return nil, false, fmt.Errorf("failed to read host key at %s: %w", path, readErr)
	}

	generatedPEM, err := generateHostKeyPEM()
	if err != nil {
		return nil, false, err
	}

	if mkErr := os.MkdirAll(filepath.Dir(path), hostKeyDirMode); mkErr != nil {
		return nil, false, fmt.Errorf("failed to create host key directory: %w", mkErr)
	}
	if writeErr := os.WriteFile(path, generatedPEM, hostKeyFileMode); writeErr != nil {
		return nil, false, fmt.Errorf("failed to write host key to %s: %w", path, writeErr)
	}

	parsed, err := gossh.ParsePrivateKey(generatedPEM)
	if err != nil {
		return nil, false, fmt.Errorf("failed to parse generated host key: %w", err)
	}
	return parsed, true, nil
}

// generateHostKeyPEM creates a new ed25519 private key in PKCS#8 PEM form.
func generateHostKeyPEM() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate host key: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal host key: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// setWinsize propagates terminal resize events to the pty.
func setWinsize(f *os.File, w, h int) {
	winsize := struct{ h, w, x, y uint16 }{uint16(h), uint16(w), 0, 0}
	_, _, _ = syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(&winsize)))
}
