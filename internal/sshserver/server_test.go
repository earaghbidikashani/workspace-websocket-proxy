/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	gossh "golang.org/x/crypto/ssh"
)

func testLogger() logr.Logger {
	zapLog, _ := zap.NewDevelopment()
	return zapr.NewLogger(zapLog)
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		ListenAddr:  "127.0.0.1:2222",
		HostKeyPath: filepath.Join(t.TempDir(), "host_key"),
		MaxSessions: 2,
	}
}

func TestNewRejectsNonLoopbackBind(t *testing.T) {
	config := testConfig(t)
	config.ListenAddr = bindAnyIPv4 + ":2222"

	if _, err := New(config, testLogger()); err == nil {
		t.Fatal("expected New to refuse a non-loopback bind")
	}
}

func TestNewAllowsNonLoopbackBindWithOverride(t *testing.T) {
	config := testConfig(t)
	config.ListenAddr = bindAnyIPv4 + ":2222"
	config.AllowNonLoopback = true

	if _, err := New(config, testLogger()); err != nil {
		t.Fatalf("expected New to accept an explicit override, got %v", err)
	}
}

func TestNewRegistersReversePortForwardingCallback(t *testing.T) {
	server, err := New(testConfig(t), testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if server.ssh.ReversePortForwardingCallback == nil {
		t.Fatal("ReversePortForwardingCallback must be set or every reverse forward is rejected")
	}
	if server.ssh.LocalPortForwardingCallback == nil {
		t.Fatal("LocalPortForwardingCallback must be set")
	}
}

func TestNewRegistersHandlers(t *testing.T) {
	server, err := New(testConfig(t), testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, name := range []string{"session", "direct-tcpip"} {
		if _, ok := server.ssh.ChannelHandlers[name]; !ok {
			t.Errorf("expected channel handler %q", name)
		}
	}
	for _, name := range []string{"tcpip-forward", "cancel-tcpip-forward"} {
		if _, ok := server.ssh.RequestHandlers[name]; !ok {
			t.Errorf("expected request handler %q", name)
		}
	}
	if _, ok := server.ssh.SubsystemHandlers["sftp"]; !ok {
		t.Error("expected sftp subsystem handler")
	}
}

func TestForwardCallbacksRestrictToLoopback(t *testing.T) {
	server, err := New(testConfig(t), testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name string
		host string
		want bool
	}{
		{"loopback ipv4", "127.0.0.1", true},
		{"loopback ipv6", "::1", true},
		{localhostHost, localhostHost, true},
		{"wildcard", bindAnyIPv4, false},
		{"routable", "10.0.0.5", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := server.allowLocalForward(nil, tc.host, 8888); got != tc.want {
				t.Errorf("allowLocalForward(%q) = %v, want %v", tc.host, got, tc.want)
			}
			if got := server.allowReverseForward(nil, tc.host, 8888); got != tc.want {
				t.Errorf("allowReverseForward(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// The callback is fail-closed: loopbackForwardHandler resolves a wildcard to a
// concrete loopback address before calling it, so an empty address reaching here
// would be a bug and must not be treated as loopback.
func TestAllowReverseForwardRejectsWildcardBind(t *testing.T) {
	server, err := New(testConfig(t), testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, bindAddr := range []string{"", bindAnyWildcard, bindAnyIPv4, bindAnyIPv6} {
		if server.allowReverseForward(nil, bindAddr, 8888) {
			t.Errorf("expected bind address %q to be refused by the callback", bindAddr)
		}
	}
}

// startTestServer runs a real SSH server on an ephemeral loopback port. The
// listener is created first because Config.validate rejects port 0.
func startTestServer(t *testing.T) (*Server, string) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve a port: %v", err)
	}

	config := testConfig(t)
	config.ListenAddr = listener.Addr().String()

	server, err := New(config, testLogger())
	if err != nil {
		_ = listener.Close()
		t.Fatalf("failed to create the server: %v", err)
	}

	go func() { _ = server.ssh.Serve(listener) }()
	t.Cleanup(func() { _ = server.ssh.Close() })

	// Serve registers its listener asynchronously, and the socket is already
	// bound, so dialing proves nothing on its own: the kernel would accept from
	// the backlog regardless. A completed handshake is what shows Serve has
	// reached its accept loop. Without this, a Shutdown can run first and find no
	// listener to close, and the library then clears its done channel when the
	// listener finally registers, leaving Serve blocked in Accept.
	warmup := dialTestClient(t, config.ListenAddr)
	_ = warmup.Close()

	return server, config.ListenAddr
}

func dialTestClient(t *testing.T, addr string) *gossh.Client {
	t.Helper()

	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "workspace",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("failed to connect to the server: %v", err)
	}
	return client
}

// startSilentCommand runs a command that records its own PID and then produces no
// further output, which is the case that used to hang teardown.
func startSilentCommand(t *testing.T, client *gossh.Client, withPty bool) (pid int) {
	t.Helper()

	pidFile := filepath.Join(t.TempDir(), "child.pid")

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("failed to open a session: %v", err)
	}

	if withPty {
		if err := session.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err != nil {
			t.Fatalf("failed to request a pty: %v", err)
		}
	}

	if err := session.Start(fmt.Sprintf("echo $$ > %s; exec sleep 300", pidFile)); err != nil {
		t.Fatalf("failed to start the command: %v", err)
	}

	// The session is deliberately not closed or waited on: the caller's point is
	// to abandon it while the command is still running.
	return waitForPidFile(t, pidFile)
}

func waitForPidFile(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if contents, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(contents))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("the command never recorded its pid in %s", path)
	return 0
}

// processAlive reports whether pid still names a live process. Signal 0 performs
// only the permission and existence checks.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func waitForCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

// A client that disappears must not leave its processes running and must not
// leak the session slot. The output copy blocks reading from a silent child, so
// before the disconnect watcher existed this hung until the pod restarted.
func TestSessionProcessesDieOnClientDisconnect(t *testing.T) {
	tests := []struct {
		name    string
		withPty bool
	}{
		{"pty path", true},
		{"exec path", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server, addr := startTestServer(t)
			client := dialTestClient(t, addr)

			pid := startSilentCommand(t, client, tc.withPty)

			if !processAlive(pid) {
				t.Fatalf("precondition: the child %d should be running", pid)
			}
			if got := server.sessions.Load(); got != 1 {
				t.Fatalf("precondition: expected 1 active session, got %d", got)
			}

			// Drop the client without letting the command finish.
			_ = client.Close()

			waitForCondition(t, fmt.Sprintf("child %d to exit", pid), func() bool {
				return !processAlive(pid)
			})
			waitForCondition(t, "the session slot to be released", func() bool {
				return server.sessions.Load() == 0
			})
		})
	}
}

// The disconnect watcher signals the process group by negative PID, which only
// reaches the child if the child leads its own group. pty.Start arranges that on
// the PTY path; the exec path needs Setpgid, and without it the signal fails with
// ESRCH and the fix silently does nothing.
func TestSessionChildLeadsItsOwnProcessGroup(t *testing.T) {
	for _, withPty := range []bool{true, false} {
		name := "exec path"
		if withPty {
			name = "pty path"
		}

		t.Run(name, func(t *testing.T) {
			_, addr := startTestServer(t)
			client := dialTestClient(t, addr)
			defer func() { _ = client.Close() }()

			pid := startSilentCommand(t, client, withPty)

			pgid, err := syscall.Getpgid(pid)
			if err != nil {
				t.Fatalf("failed to read the process group of %d: %v", pid, err)
			}
			if pgid != pid {
				t.Errorf("child %d is in process group %d, so a negative-PID signal "+
					"would miss it; expected the child to lead its own group", pid, pgid)
			}
			if selfPgid, _ := syscall.Getpgid(os.Getpid()); pgid == selfPgid {
				t.Errorf("child shares the server's process group %d, so signalling "+
					"the group would target the server itself", selfPgid)
			}
		})
	}
}

// The underlying Shutdown waits indefinitely when its context has no deadline,
// so an idle client must not be able to hold termination open. Shutdown has to
// give up at the deadline and sever what is left rather than returning the
// context error and leaving the connection intact.
func TestShutdownFallsBackToCloseAfterDrainDeadline(t *testing.T) {
	server, addr := startTestServer(t)

	client := dialTestClient(t, addr)
	defer func() { _ = client.Close() }()

	// Hold a session open so the drain has something to wait for.
	pid := startSilentCommand(t, client, false)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("expected Shutdown to recover by closing connections, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Shutdown took %s, which suggests it waited without a deadline", elapsed)
	}

	// Severing the connection cancels the session context, so the session's
	// processes are cleaned up rather than left behind.
	waitForCondition(t, fmt.Sprintf("child %d to exit after shutdown", pid), func() bool {
		return !processAlive(pid)
	})
}

func TestShutdownReturnsPromptlyWithNoConnections(t *testing.T) {
	server, _ := startTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("expected a clean shutdown, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Shutdown with no connections took %s", elapsed)
	}
}

func TestSessionCapacity(t *testing.T) {
	server := &Server{config: &Config{MaxSessions: 2}, logger: testLogger()}

	if !server.acquireSession() {
		t.Fatal("expected the first acquisition to succeed")
	}
	if !server.acquireSession() {
		t.Fatal("expected the second acquisition to succeed")
	}
	if server.acquireSession() {
		t.Fatal("expected the third acquisition to be refused")
	}

	server.releaseSession()
	if !server.acquireSession() {
		t.Fatal("expected an acquisition to succeed after a release")
	}
}

func TestSessionCapacityUnlimitedWhenZero(t *testing.T) {
	server := &Server{config: &Config{MaxSessions: 0}, logger: testLogger()}

	for i := 0; i < 50; i++ {
		if !server.acquireSession() {
			t.Fatalf("expected acquisition %d to succeed with an unlimited cap", i)
		}
	}
	if got := server.sessions.Load(); got != 0 {
		t.Errorf("expected no accounting with an unlimited cap, got %d", got)
	}
}

func TestLoadOrCreateHostKeyPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "host_key")

	first, generated, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !generated {
		t.Error("expected the first call to generate a key")
	}

	second, generated, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated {
		t.Error("expected the second call to reuse the persisted key")
	}

	if gossh.FingerprintSHA256(first.PublicKey()) != gossh.FingerprintSHA256(second.PublicKey()) {
		t.Error("expected a stable host key fingerprint across restarts")
	}
}

func TestLoadOrCreateHostKeyUsesEd25519(t *testing.T) {
	signer, _, err := loadOrCreateHostKey(filepath.Join(t.TempDir(), "host_key"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want := "ssh-ed25519"; signer.PublicKey().Type() != want {
		t.Errorf("expected %s, got %s", want, signer.PublicKey().Type())
	}
}

func TestLoadOrCreateHostKeyRestrictsPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "host_key")

	if _, _, err := loadOrCreateHostKey(path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := info.Mode().Perm(); got != hostKeyFileMode {
		t.Errorf("expected mode %o, got %o", hostKeyFileMode, got)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != hostKeyDirMode {
		t.Errorf("expected dir mode %o, got %o", hostKeyDirMode, got)
	}
}

func TestLoadOrCreateHostKeyRejectsCorruptKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_key")
	if err := os.WriteFile(path, []byte("not a private key"), hostKeyFileMode); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, _, err := loadOrCreateHostKey(path); err == nil {
		t.Fatal("expected an error for an unreadable host key")
	}
}

func TestBuildShellCommand(t *testing.T) {
	tests := []struct {
		name       string
		loginShell bool
		rawCmd     string
		wantArgs   []string
	}{
		{
			name:     "interactive",
			wantArgs: []string{shellBash},
		},
		{
			name:       "interactive login",
			loginShell: true,
			wantArgs:   []string{shellBash, "-l"},
		},
		{
			name:     "command",
			rawCmd:   "echo hello",
			wantArgs: []string{shellBash, "-c", "echo hello"},
		},
		{
			name:       "command login",
			loginShell: true,
			rawCmd:     "echo hello",
			wantArgs:   []string{shellBash, "-lc", "echo hello"},
		},
		{
			name:     "whitespace only command treated as interactive",
			rawCmd:   "   ",
			wantArgs: []string{shellBash},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := buildShellCommand(shellBash, tc.loginShell, tc.rawCmd)
			if !reflect.DeepEqual(cmd.Args, tc.wantArgs) {
				t.Errorf("expected args %v, got %v", tc.wantArgs, cmd.Args)
			}
		})
	}
}

func TestResolveShellPrefersShellEnv(t *testing.T) {
	t.Setenv("SHELL", shellSh)

	if got := resolveShell(); got != shellSh {
		t.Errorf("expected /bin/sh, got %s", got)
	}
}

func TestResolveShellFallsBackWhenShellEnvMissing(t *testing.T) {
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "does-not-exist"))

	got := resolveShell()
	if got != shellBash && got != shellSh {
		t.Errorf("expected a fallback shell, got %s", got)
	}
}

func TestMergeEnvSessionValuesWin(t *testing.T) {
	t.Setenv("SSHSERVER_TEST_KEY", "from-process")

	merged := mergeEnv([]string{"SSHSERVER_TEST_KEY=from-session"})

	if !slices.Contains(merged, "SSHSERVER_TEST_KEY=from-session") {
		t.Error("expected the session value to win")
	}
	if slices.Contains(merged, "SSHSERVER_TEST_KEY=from-process") {
		t.Error("expected the process value to be replaced")
	}
}

func TestMergeEnvKeepsProcessEnvironment(t *testing.T) {
	t.Setenv("SSHSERVER_TEST_KEEP", "kept")

	merged := mergeEnv(nil)

	if !slices.Contains(merged, "SSHSERVER_TEST_KEEP=kept") {
		t.Error("expected process environment entries to be preserved")
	}
}

// supervisord starts a program under user= without setting HOME or USER, so the
// server can inherit an environment that has neither. A session must still land
// in a home directory, because IDEs install their server component under $HOME.
func TestWithPasswdFallbackFillsMissingAccountVariables(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skipf("cannot resolve the current user: %v", err)
	}

	filled := withPasswdFallback([]string{"PATH=/usr/bin"})

	if home, ok := lookupEnv(filled, envHome); !ok || home != current.HomeDir {
		t.Errorf("expected HOME=%q from the passwd entry, got %q (found=%v)",
			current.HomeDir, home, ok)
	}
	for _, key := range []string{envUser, envLogname} {
		if value, ok := lookupEnv(filled, key); !ok || value != current.Username {
			t.Errorf("expected %s=%q from the passwd entry, got %q (found=%v)",
				key, current.Username, value, ok)
		}
	}
}

func TestWithPasswdFallbackLeavesExistingValuesAlone(t *testing.T) {
	env := []string{
		envHome + "=/home/client",
		envUser + "=client",
		envLogname + "=client",
	}

	filled := withPasswdFallback(slices.Clone(env))

	if !slices.Equal(filled, env) {
		t.Errorf("expected the environment to be untouched, got %v", filled)
	}
}

func TestSessionWorkingDir(t *testing.T) {
	existing := t.TempDir()

	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"existing home", []string{envHome + "=" + existing}, existing},
		{"missing home", []string{"PATH=/usr/bin"}, ""},
		{"empty home", []string{envHome + "="}, ""},
		{"home that does not exist", []string{envHome + "=" + existing + "/nope"}, ""},
		{"home that is a file", []string{envHome + "=" + writeTempFile(t, existing)}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionWorkingDir(tc.env); got != tc.want {
				t.Errorf("sessionWorkingDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

func writeTempFile(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("failed to create the fixture file: %v", err)
	}
	return path
}

func TestLookupEnv(t *testing.T) {
	env := []string{"HOME=/home/jovyan", "PATH=/usr/bin"}

	if value, ok := lookupEnv(env, "HOME"); !ok || value != "/home/jovyan" {
		t.Errorf("expected /home/jovyan, got %q (found=%v)", value, ok)
	}
	if _, ok := lookupEnv(env, "MISSING"); ok {
		t.Error("expected MISSING to be absent")
	}
}

func TestExitCodeSuccess(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestExitCodeNonExitError(t *testing.T) {
	if got := exitCode(os.ErrNotExist); got != exitCodeGeneralError {
		t.Errorf("expected %d, got %d", exitCodeGeneralError, got)
	}
}

func TestExitCodePropagatesCommandStatus(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not available")
	}

	tests := []struct {
		name   string
		script string
		want   int
	}{
		{name: "zero", script: "exit 0", want: 0},
		{name: "non-zero", script: "exit 42", want: 42},
		{name: "signalled", script: "kill -TERM $$", want: signalExitCodeBase + 15},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(shell, "-c", tc.script)
			if got := exitCode(cmd.Run()); got != tc.want {
				t.Errorf("expected %d, got %d", tc.want, got)
			}
		})
	}
}
