/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshserver

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	config := LoadConfig()

	if want := "127.0.0.1:2222"; config.ListenAddr != want {
		t.Errorf("expected %s, got %s", want, config.ListenAddr)
	}
	if want := filepath.Join(home, hostKeyDirName, hostKeyFileName); config.HostKeyPath != want {
		t.Errorf("expected %s, got %s", want, config.HostKeyPath)
	}
	if config.IdleTimeout != 0 {
		t.Errorf("expected 0, got %s", config.IdleTimeout)
	}
	if config.MaxSessions != DefaultMaxSessions {
		t.Errorf("expected %d, got %d", DefaultMaxSessions, config.MaxSessions)
	}
	if config.LoginShell {
		t.Error("expected LoginShell to default to false")
	}
	if config.AllowNonLoopback {
		t.Error("expected AllowNonLoopback to default to false")
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("SSH_LISTEN_ADDR", "127.0.0.1:2020")
	t.Setenv("SSH_HOST_KEY_PATH", "/var/lib/keys/host_key")
	t.Setenv("SSH_IDLE_TIMEOUT", "30m")
	t.Setenv("SSH_MAX_SESSIONS", "3")
	t.Setenv("SSH_LOGIN_SHELL", "true")
	t.Setenv("SSH_ALLOW_NON_LOOPBACK", "true")

	config := LoadConfig()

	if want := "127.0.0.1:2020"; config.ListenAddr != want {
		t.Errorf("expected %s, got %s", want, config.ListenAddr)
	}
	if want := "/var/lib/keys/host_key"; config.HostKeyPath != want {
		t.Errorf("expected %s, got %s", want, config.HostKeyPath)
	}
	if want := 30 * time.Minute; config.IdleTimeout != want {
		t.Errorf("expected %s, got %s", want, config.IdleTimeout)
	}
	if config.MaxSessions != 3 {
		t.Errorf("expected 3, got %d", config.MaxSessions)
	}
	if !config.LoginShell {
		t.Error("expected LoginShell to be true")
	}
	if !config.AllowNonLoopback {
		t.Error("expected AllowNonLoopback to be true")
	}
}

func TestLoadConfigIgnoresUnparseableValues(t *testing.T) {
	t.Setenv("SSH_IDLE_TIMEOUT", "not-a-duration")
	t.Setenv("SSH_MAX_SESSIONS", "not-a-number")
	t.Setenv("SSH_LOGIN_SHELL", "not-a-bool")

	config := LoadConfig()

	if config.IdleTimeout != 0 {
		t.Errorf("expected fallback 0, got %s", config.IdleTimeout)
	}
	if config.MaxSessions != DefaultMaxSessions {
		t.Errorf("expected fallback %d, got %d", DefaultMaxSessions, config.MaxSessions)
	}
	if config.LoginShell {
		t.Error("expected fallback false")
	}
}

func TestDefaultHostKeyPathWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")

	if path := DefaultHostKeyPath(); path != "" {
		t.Errorf("expected empty path when home is unset, got %s", path)
	}
}

func validConfig() *Config {
	return &Config{
		ListenAddr:  "127.0.0.1:2222",
		HostKeyPath: "/var/lib/keys/host_key",
		MaxSessions: 10,
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:   "loopback ipv4",
			mutate: func(c *Config) { c.ListenAddr = "127.0.0.1:2222" },
		},
		{
			name:   "loopback ipv6",
			mutate: func(c *Config) { c.ListenAddr = "[::1]:2222" },
		},
		{
			name:   localhostHost,
			mutate: func(c *Config) { c.ListenAddr = localhostHost + ":2222" },
		},
		{
			name:    "wildcard bind rejected",
			mutate:  func(c *Config) { c.ListenAddr = "0.0.0.0:2222" },
			wantErr: true,
		},
		{
			name:    "empty host rejected",
			mutate:  func(c *Config) { c.ListenAddr = ":2222" },
			wantErr: true,
		},
		{
			name:    "routable address rejected",
			mutate:  func(c *Config) { c.ListenAddr = "10.0.0.5:2222" },
			wantErr: true,
		},
		{
			name: "routable address allowed with explicit override",
			mutate: func(c *Config) {
				c.ListenAddr = "0.0.0.0:2222"
				c.AllowNonLoopback = true
			},
		},
		{
			name:    "malformed address rejected",
			mutate:  func(c *Config) { c.ListenAddr = "127.0.0.1" },
			wantErr: true,
		},
		{
			name:    "port zero rejected",
			mutate:  func(c *Config) { c.ListenAddr = "127.0.0.1:0" },
			wantErr: true,
		},
		{
			name:    "port above range rejected",
			mutate:  func(c *Config) { c.ListenAddr = "127.0.0.1:70000" },
			wantErr: true,
		},
		{
			name:    "non-numeric port rejected",
			mutate:  func(c *Config) { c.ListenAddr = "127.0.0.1:ssh" },
			wantErr: true,
		},
		{
			name:    "empty host key path rejected",
			mutate:  func(c *Config) { c.HostKeyPath = "" },
			wantErr: true,
		},
		{
			name:    "relative host key path rejected",
			mutate:  func(c *Config) { c.HostKeyPath = "keys/host_key" },
			wantErr: true,
		},
		{
			name:    "negative max sessions rejected",
			mutate:  func(c *Config) { c.MaxSessions = -1 },
			wantErr: true,
		},
		{
			name:   "zero max sessions allowed",
			mutate: func(c *Config) { c.MaxSessions = 0 },
		},
		{
			name:    "negative idle timeout rejected",
			mutate:  func(c *Config) { c.IdleTimeout = -time.Second },
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := validConfig()
			tc.mutate(config)

			err := config.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.53", true},
		{"::1", true},
		{localhostHost, true},
		{"", false},
		{"*", false},
		{"0.0.0.0", false},
		{"10.0.0.5", false},
		{"::", false},
		{"example.com", false},
	}

	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			if got := isLoopback(tc.host); got != tc.want {
				t.Errorf("isLoopback(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestHasEphemeralHostKeyPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/tmp/ssh_host_ed25519_key", true},
		{"/var/tmp/ssh_host_ed25519_key", true},
		{"/run/ssh_host_ed25519_key", true},
		{"/dev/shm/ssh_host_ed25519_key", true},
		{"/home/jovyan/.jupyter-k8s/ssh_host_ed25519_key", false},
		{"/var/lib/keys/host_key", false},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			config := &Config{HostKeyPath: tc.path}
			if got := config.HasEphemeralHostKeyPath(); got != tc.want {
				t.Errorf("HasEphemeralHostKeyPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
