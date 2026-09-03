/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshserver

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

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
	config.ListenAddr = "0.0.0.0:2222"

	if _, err := New(config, testLogger()); err == nil {
		t.Fatal("expected New to refuse a non-loopback bind")
	}
}

func TestNewAllowsNonLoopbackBindWithOverride(t *testing.T) {
	config := testConfig(t)
	config.ListenAddr = "0.0.0.0:2222"
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
		{"wildcard", "0.0.0.0", false},
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

func TestAllowReverseForwardAcceptsImplicitBind(t *testing.T) {
	server, err := New(testConfig(t), testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !server.allowReverseForward(nil, "", 8888) {
		t.Error("an empty bind address means loopback and must be accepted")
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
