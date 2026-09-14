/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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

	// killGracePeriod is how long a disconnected session's processes are given
	// to exit after SIGHUP before they are killed outright.
	killGracePeriod = 5 * time.Second

	// Global request types from RFC 4254 section 7.1.
	requestTypeForward       = "tcpip-forward"
	requestTypeCancelForward = "cancel-tcpip-forward"

	// Bind addresses that select every interface rather than one.
	bindAnyWildcard = "*"
	bindAnyIPv4     = "0.0.0.0"
	bindAnyIPv6     = "::"
)

// remoteForwardRequest mirrors the wire format that RFC 4254 section 7.1 defines
// for both tcpip-forward and cancel-tcpip-forward. The library keeps its own copy
// of this struct unexported, so the fields are redeclared here for
// loopbackForwardHandler to read.
type remoteForwardRequest struct {
	BindAddr string
	BindPort uint32
}

// anyBindAddresses are the bind addresses that mean "every interface" once they
// reach net.Listen.
var anyBindAddresses = map[string]bool{
	"":              true,
	bindAnyWildcard: true,
	bindAnyIPv4:     true,
	bindAnyIPv6:     true,
}

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
	forwardHandler := newLoopbackForwardHandler(logger)

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
			requestTypeForward:       forwardHandler.HandleSSHRequest,
			requestTypeCancelForward: forwardHandler.HandleSSHRequest,
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

// Shutdown closes the listener and drains active connections until ctx expires,
// then severs whatever is left.
//
// Draining matters on a pod rollout: an abrupt close would cut a session
// mid-command or mid-SFTP write, which can leave a truncated file. The fallback
// matters just as much, because the underlying Shutdown waits indefinitely when
// its context has no deadline, so one lingering session would otherwise hold
// termination open until the kubelet sends SIGKILL.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.ssh.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		s.logger.Info("Drain deadline passed, closing active connections", "error", err.Error())
		return s.ssh.Close()
	}
	return err
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
//
// loopbackForwardHandler resolves a wildcard request to loopback before calling
// this, so every address reaching here is concrete and anything that is not
// loopback is refused.
func (s *Server) allowReverseForward(_ ssh.Context, bindHost string, bindPort uint32) bool {
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
	cmd.Env = withPasswdFallback(mergeEnv(session.Environ()))
	cmd.Dir = sessionWorkingDir(cmd.Env)

	sc := &sessionCmd{cmd: cmd}

	ptyReq, winCh, isPty := session.Pty()
	if isPty {
		s.runWithPty(session, sc, ptyReq, winCh, logger)
		return
	}
	s.runWithoutPty(session, sc, logger)
}

// sessionCmd owns the process started for a session and serialises signalling it
// against reaping it.
//
// The serialisation is not incidental. Once Wait has reaped the child, the
// kernel is free to reuse its PID, so a signal sent after the reap could land on
// an unrelated process group. Making that impossible is cheaper than reasoning
// about how unlikely it is.
type sessionCmd struct {
	cmd *exec.Cmd

	mu     sync.Mutex
	reaped bool
}

// signalGroup sends sig to the whole process group of the session's process, so
// that processes the session backgrounded are included. It is a no-op once the
// process has been reaped.
func (sc *sessionCmd) signalGroup(sig syscall.Signal) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.reaped || sc.cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-sc.cmd.Process.Pid, sig)
}

// wait reaps the process and marks it reaped so no later signal can be sent.
func (sc *sessionCmd) wait() error {
	err := sc.cmd.Wait()

	sc.mu.Lock()
	sc.reaped = true
	sc.mu.Unlock()

	return err
}

// terminateOnDisconnect stops the session's processes if the client goes away
// before the command exits, and returns a function that cancels the watch.
//
// Without this a session can outlive its client indefinitely. The output copy
// blocks reading from the child, and a child that is silent because it is sitting
// at an idle prompt never unblocks it, so the process is never signalled, Wait
// never returns and the session's slot is held for the lifetime of the pod. An
// interactive shell left open on a laptop that goes to sleep is exactly that
// case, and it is the ordinary one rather than an unusual one.
//
// Signalling the group also releases the blocked copy for free: when the last
// process holding the pty slave exits, the read on the master fails and teardown
// proceeds normally.
func (s *Server) terminateOnDisconnect(session ssh.Session, sc *sessionCmd, logger logr.Logger) (cancel func()) {
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		select {
		case <-done:
			return
		case <-session.Context().Done():
		}

		logger.Info("Client disconnected, hanging up the session process group")
		if err := sc.signalGroup(syscall.SIGHUP); err != nil {
			logger.V(1).Info("Failed to hang up the session process group", "error", err.Error())
		}

		select {
		case <-done:
		case <-time.After(killGracePeriod):
			logger.Info("Session process group outlived the grace period, killing it")
			if err := sc.signalGroup(syscall.SIGKILL); err != nil {
				logger.V(1).Info("Failed to kill the session process group", "error", err.Error())
			}
		}
	}()

	return func() {
		close(done)
		<-finished
	}
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
// Teardown order matters. watchWindowSize holds the same *os.File and resizes it,
// and os.File reference-counts Read and Write but not Fd, so closing the master
// while a resize is in flight is a use-after-close that the race detector
// reports. The resize goroutine is therefore stopped and joined before the master
// is closed.
//
// pty.Start puts the child in a new session with the pty as its controlling
// terminal, which makes it a process group leader and lets
// terminateOnDisconnect signal the group.
func (s *Server) runWithPty(
	session ssh.Session,
	sc *sessionCmd,
	ptyReq ssh.Pty,
	winCh <-chan ssh.Window,
	logger logr.Logger,
) {
	cmd := sc.cmd
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

	stopWatch := s.terminateOnDisconnect(session, sc, logger)
	defer stopWatch()

	s.setWinsize(f, ptyReq.Window, logger)
	stopResize, resizeDone := s.watchWindowSize(f, winCh, logger)

	go func() {
		_, _ = io.Copy(f, session)
	}()
	_, _ = io.Copy(session, f)

	close(stopResize)
	<-resizeDone
	_ = f.Close()

	s.finish(session, sc, logger)
}

// watchWindowSize applies terminal resizes the client sends mid-session, so a
// full-screen program redraws correctly when the IDE's terminal pane changes
// size. It returns a channel the caller closes to stop the goroutine and a
// second channel the goroutine closes once it has returned; the caller must
// wait on the latter before closing f.
func (s *Server) watchWindowSize(
	f *os.File,
	winCh <-chan ssh.Window,
	logger logr.Logger,
) (stop chan struct{}, done chan struct{}) {
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
				s.setWinsize(f, win, logger)
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
//
// Setpgid is required here, and only here. On the PTY path pty.Start already puts
// the child in its own session, but a plain Start leaves it in the server's
// process group, where signalling the group by negative PID fails with ESRCH and
// terminateOnDisconnect would silently do nothing.
func (s *Server) runWithoutPty(session ssh.Session, sc *sessionCmd, logger logr.Logger) {
	cmd := sc.cmd
	cmd.Stdout = session
	cmd.Stderr = session.Stderr()

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true

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

	stopWatch := s.terminateOnDisconnect(session, sc, logger)
	defer stopWatch()

	go func() {
		_, _ = io.Copy(stdin, session)
		_ = stdin.Close()
	}()

	s.finish(session, sc, logger)
}

// finish reaps the child and reports its status to the client as the channel's
// exit-status request.
func (s *Server) finish(session ssh.Session, sc *sessionCmd, logger logr.Logger) {
	code := exitCode(sc.wait())
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

// setWinsize tells the pty its dimensions, which is what makes full-screen
// programs in the session lay out correctly. A failed resize is logged rather
// than fatal: the session remains usable, it is only drawn at the wrong size.
func (s *Server) setWinsize(f *os.File, win ssh.Window, logger logr.Logger) {
	size := &pty.Winsize{Rows: uint16(win.Height), Cols: uint16(win.Width)}
	if err := pty.Setsize(f, size); err != nil {
		logger.V(1).Info("Failed to resize pty",
			"rows", win.Height, "cols", win.Width, "error", err.Error())
	}
}
