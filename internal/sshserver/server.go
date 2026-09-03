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
	exitCodeGeneralError = 1

	exitCodeCommandNotExecutable = 127

	signalExitCodeBase = 128

	hostKeyDirMode  = 0o700
	hostKeyFileMode = 0o600

	shellBash = "/bin/bash"
	shellSh   = "/bin/sh"
)

// Server is the SSH server that terminates remote IDE sessions.
type Server struct {
	config   *Config
	logger   logr.Logger
	ssh      *ssh.Server
	sessions atomic.Int64
}

// New validates the configuration and builds the SSH server.
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

func (s *Server) allowLocalForward(_ ssh.Context, host string, port uint32) bool {
	if !isLoopback(host) {
		s.logger.Info("Rejected local forward to non-loopback destination", "host", host, "port", port)
		return false
	}
	s.logger.V(1).Info("Accepted local forward", "host", host, "port", port)
	return true
}

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

func (s *Server) releaseSession() {
	if s.config.MaxSessions <= 0 {
		return
	}
	s.sessions.Add(-1)
}

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

func (s *Server) finish(session ssh.Session, cmd *exec.Cmd, logger logr.Logger) {
	code := exitCode(cmd.Wait())
	logger.Info("Session closed", "exitCode", code)
	_ = session.Exit(code)
}

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

func lookupEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

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

func setWinsize(f *os.File, w, h int) {
	winsize := struct{ h, w, x, y uint16 }{uint16(h), uint16(w), 0, 0}
	_, _, _ = syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(&winsize)))
}
