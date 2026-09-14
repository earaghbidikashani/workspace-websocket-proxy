/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

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
	// exitCodeGeneralError is the status reported when the server itself fails
	// before or around the command, rather than the command exiting non-zero.
	exitCodeGeneralError = 1

	// exitCodeCommandNotExecutable mirrors the shell convention for a command
	// that could not be executed at all.
	exitCodeCommandNotExecutable = 127

	// signalExitCodeBase is the shell convention for reporting a process killed
	// by signal N as exit status 128+N.
	signalExitCodeBase = 128

	hostKeyDirMode  = 0o700
	hostKeyFileMode = 0o600

	shellBash = "/bin/bash"
	shellSh   = "/bin/sh"
)

// Server is the SSH server that terminates remote IDE sessions. It runs inside
// the workspace container and listens on loopback, where the proxy sidecar
// reaches it over the pod's shared network namespace.
type Server struct {
	config *Config
	logger logr.Logger

	// ssh is the underlying gliderlabs server, configured in New.
	ssh *ssh.Server

	// sessions counts in-flight shell and exec channels against MaxSessions.
	sessions atomic.Int64
}

// New validates the configuration and builds the SSH server.
//
// No authentication handler is registered, so gliderlabs accepts every client.
// That is deliberate: the ingress validates a short-lived token before any byte
// reaches the proxy, and the loopback bind that Config.validate enforces is what
// keeps the socket unreachable from outside the pod.
func New(config *Config, logger logr.Logger) (*Server, error) {
	if err := config.validate(); err != nil {
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

	if config.hasEphemeralHostKeyPath() {
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

// ListenAndServe starts the SSH server and blocks until it stops, returning
// ssh.ErrServerClosed after a Shutdown.
func (s *Server) ListenAndServe() error {
	s.logger.Info("Starting SSH server",
		"addr", s.config.ListenAddr,
		"maxSessions", s.config.MaxSessions,
		"idleTimeout", s.config.IdleTimeout,
		"loginShell", s.config.LoginShell,
		"authentication", "none (loopback only, authenticated at the ingress)")
	return s.ssh.ListenAndServe()
}

// Shutdown closes the listener and terminates active connections immediately.
// The context is accepted for interface symmetry with the proxy server and is
// not yet honoured as a drain deadline.
func (s *Server) Shutdown(_ context.Context) error {
	return s.ssh.Close()
}

// allowLocalForward authorizes a "direct-tcpip" channel, the forward a client
// opens to reach a port inside the pod (ssh -L). Only loopback destinations are
// permitted, so a session cannot use the pod as a jump host into the cluster
// network.
func (s *Server) allowLocalForward(_ ssh.Context, host string, port uint32) bool {
	if !isLoopback(host) {
		s.logger.Info("Rejected local forward to non-loopback destination", "host", host, "port", port)
		return false
	}
	s.logger.V(1).Info("Accepted local forward", "host", host, "port", port)
	return true
}

// allowReverseForward authorizes a "tcpip-forward" request, where the client
// asks the server to listen on a port and relay inbound connections back over
// the session (ssh -R). IDEs use this for feature forwarding. Only loopback
// bind addresses are permitted so the listener is not reachable from outside
// the pod.
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

// handleSession services one "session" channel: the interactive shell or exec
// command an IDE runs. It claims a session slot, resolves the shell and
// environment, then dispatches to the PTY or non-PTY path depending on whether
// the client requested a terminal.
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

// acquireSession claims one of the MaxSessions slots, reporting false when the
// server is already at capacity. A non-positive MaxSessions disables the limit.
// The count covers shell and exec channels only, not SFTP or port forwards.
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

// releaseSession returns a slot claimed by acquireSession.
func (s *Server) releaseSession() {
	if s.config.MaxSessions <= 0 {
		return
	}
	s.sessions.Add(-1)
}

// runWithPty runs the command on a pseudo-terminal, the path an interactive
// shell takes.
//
// The pty master is a single file descriptor carrying both directions, so there
// is one pipe rather than the three of runWithoutPty: a goroutine copies client
// input into the master while the caller's goroutine copies program output back
// out. The outbound copy returns when the child exits and the kernel reports EIO
// on the master, which is what ends the session.
//
// Teardown order matters. watchWindowSize holds the same *os.File and calls
// setWinsize on it, and os.File reference-counts Read and Write but not Fd, so
// closing the master while a resize is in flight is a use-after-close that the
// race detector reports. The resize goroutine is therefore stopped and joined
// before the master is closed.
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

// watchWindowSize applies terminal resizes the client sends mid-session, so a
// full-screen program redraws correctly when the IDE's terminal pane changes
// size. It returns a channel the caller closes to stop the goroutine and a
// second channel the goroutine closes once it has returned; the caller must
// wait on the latter before closing f.
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

// runWithoutPty runs the command with ordinary pipes, the path an exec command
// takes when the client requested no terminal. This is how IDEs bootstrap their
// server component, so stdout and stderr stay separate and the exit status is
// propagated verbatim.
//
// Stdout and stderr are wired straight to the session. Stdin needs a pipe
// because the copy has to be closed once the client stops sending: a program
// reading to EOF would otherwise block forever.
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

// finish reaps the child and reports its status to the client as the channel's
// exit-status request.
func (s *Server) finish(session ssh.Session, cmd *exec.Cmd, logger logr.Logger) {
	code := exitCode(cmd.Wait())
	logger.Info("Session closed", "exitCode", code)
	_ = session.Exit(code)
}

// exitCode translates the error from exec.Cmd.Wait into the status an SSH
// client expects, reporting a signalled process as 128+signal. IDEs branch on
// this value while bootstrapping, so a swallowed status surfaces as a
// connection failure that is hard to diagnose from the client.
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

// handleSFTP serves the "sftp" subsystem, which is how an IDE browses and
// transfers files. The session channel is handed to the SFTP server as its
// transport, so the protocol runs inside the same SSH connection as the shell.
// Requests are served with the process's own credentials; there is no
// chroot, so reachable paths are whatever the container's user can open.
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

// buildShellCommand assembles the command for a session: the shell on its own
// for an interactive login, or the shell with -c for an exec request. The
// command always goes through a shell so that quoting, pipes and redirection in
// what the client sent behave the way the client expects.
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

// resolveShell picks the shell to run, preferring $SHELL and falling back to
// bash then sh. Workspace images vary, so the candidates are probed rather than
// assumed.
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

// mergeEnv combines the server process's environment with the variables the
// client sent, letting the client win on conflicts. The process environment is
// the base because it carries the image's own setup, such as PATH and the
// virtualenv, that the workspace needs.
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

// lookupEnv reads a variable out of an environment slice, for the merged
// environment that has not been applied to the process.
func lookupEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

// loadOrCreateHostKey returns the host key at path, generating and persisting
// one on first use, and reports whether it generated it. Persisting matters:
// with an ephemeral key the fingerprint changes on every restart and clients
// learn to disable host-key checking. A key that exists but cannot be parsed is
// an error rather than a cause to overwrite it.
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

// generateHostKeyPEM creates a new ed25519 host key as PKCS#8 PEM.
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

// setWinsize tells the pty its dimensions, which is what makes full-screen
// programs in the session lay out correctly.
func setWinsize(f *os.File, w, h int) {
	winsize := struct{ h, w, x, y uint16 }{uint16(h), uint16(w), 0, 0}
	_, _, _ = syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(&winsize)))
}
