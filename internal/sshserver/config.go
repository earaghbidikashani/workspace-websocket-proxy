/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshserver

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultPort is the loopback port the SSH server listens on. It must match the
	// target port the WebSocket proxy bridges to.
	DefaultPort = 2222

	// DefaultMaxSessions caps concurrent SSH sessions. Each session spawns a shell,
	// so an unbounded count risks exhausting the workspace container's memory.
	DefaultMaxSessions = 10

	// hostKeyDirName holds the persisted host key, relative to the user's home
	// directory. Home is backed by the workspace's storage volume, so a key written
	// there survives pod recreation rather than only container restart.
	hostKeyDirName = ".jupyter-k8s"

	// hostKeyFileName is the file name of the persisted ed25519 host key.
	hostKeyFileName = "ssh_host_ed25519_key"

	// localhostHost is the hostname form of a loopback address.
	localhostHost = "localhost"
)

// ephemeralPathPrefixes are locations that live on the container's writable layer
// rather than on a mounted volume. A host key stored there is regenerated on every
// container restart, which makes every client report a changed host key.
var ephemeralPathPrefixes = []string{"/tmp/", "/var/tmp/", "/run/", "/dev/shm/"}

// Config holds all configuration for the SSH server.
type Config struct {
	// ListenAddr is the address the SSH server listens on. It must resolve to a
	// loopback address unless AllowNonLoopback is set.
	ListenAddr string

	// HostKeyPath is where the ed25519 host key is persisted, generated on first
	// use. The path must survive container restarts: a new host key on every start
	// makes clients refuse to reconnect until their known_hosts entry is cleared.
	HostKeyPath string

	// IdleTimeout closes a session that has seen no traffic for this long. Zero
	// disables it.
	IdleTimeout time.Duration

	// MaxSessions caps concurrent SSH sessions. Zero disables the cap.
	MaxSessions int

	// LoginShell runs the session shell as a login shell. Images that put their
	// interpreter on PATH through a shell profile rather than through the image
	// environment require this; images that set PATH in the environment do not.
	LoginShell bool

	// AllowNonLoopback permits binding a non-loopback address.
	//
	// This server performs no SSH authentication. It is designed to sit behind an
	// authenticating reverse proxy: the workspace ingress validates a short-lived
	// JWT before any byte reaches the tunnel, and the only transport-level
	// protection is that the socket is unreachable from outside the pod. Binding a
	// routable address therefore publishes an unauthenticated shell, so Validate
	// refuses to do so unless this is explicitly set.
	AllowNonLoopback bool
}

// LoadConfig reads configuration from environment variables with sensible
// defaults. It does not validate: callers override fields from flags before
// handing the result to New, which validates.
func LoadConfig() *Config {
	return &Config{
		ListenAddr:       getEnv("SSH_LISTEN_ADDR", fmt.Sprintf("127.0.0.1:%d", DefaultPort)),
		HostKeyPath:      getEnv("SSH_HOST_KEY_PATH", DefaultHostKeyPath()),
		IdleTimeout:      getDurationEnv("SSH_IDLE_TIMEOUT", 0),
		MaxSessions:      getIntEnv("SSH_MAX_SESSIONS", DefaultMaxSessions),
		LoginShell:       getBoolEnv("SSH_LOGIN_SHELL", false),
		AllowNonLoopback: getBoolEnv("SSH_ALLOW_NON_LOOPBACK", false),
	}
}

// DefaultHostKeyPath returns the host key location under the user's home
// directory. It returns an empty string when the home directory cannot be
// determined, which Validate rejects so the operator is told to set
// SSH_HOST_KEY_PATH rather than silently getting an ephemeral key.
func DefaultHostKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, hostKeyDirName, hostKeyFileName)
}

// Validate rejects a configuration that would expose an unauthenticated shell or
// that cannot persist a host key.
func (c *Config) Validate() error {
	host, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("SSH_LISTEN_ADDR must be host:port, got %q: %w", c.ListenAddr, err)
	}

	if p, convErr := strconv.Atoi(port); convErr != nil || p < 1 || p > 65535 {
		return fmt.Errorf("SSH_LISTEN_ADDR port must be between 1 and 65535, got %q", port)
	}

	if c.HostKeyPath == "" {
		return fmt.Errorf(
			"host key path is empty and no home directory could be determined: " +
				"set SSH_HOST_KEY_PATH to a location on persistent storage")
	}

	if !filepath.IsAbs(c.HostKeyPath) {
		return fmt.Errorf("SSH_HOST_KEY_PATH must be an absolute path, got %q", c.HostKeyPath)
	}

	if c.MaxSessions < 0 {
		return fmt.Errorf("SSH_MAX_SESSIONS must not be negative, got %d", c.MaxSessions)
	}

	if c.IdleTimeout < 0 {
		return fmt.Errorf("SSH_IDLE_TIMEOUT must not be negative, got %s", c.IdleTimeout)
	}

	if !c.AllowNonLoopback && !isLoopback(host) {
		return fmt.Errorf(
			"refusing to listen on %q: this server performs no SSH authentication and "+
				"must stay on loopback behind the authenticating ingress; set "+
				"SSH_ALLOW_NON_LOOPBACK=true only if an equivalent authentication layer "+
				"is provably in front of it", c.ListenAddr)
	}

	return nil
}

// HasEphemeralHostKeyPath reports whether the host key would be written to the
// container's writable layer, where it is lost on restart.
func (c *Config) HasEphemeralHostKeyPath() bool {
	for _, prefix := range ephemeralPathPrefixes {
		if strings.HasPrefix(c.HostKeyPath, prefix) {
			return true
		}
	}
	return false
}

// isLoopback reports whether host is a loopback address. An empty host, or a
// wildcard, binds every interface and is therefore not loopback.
func isLoopback(host string) bool {
	if host == "" || host == "*" {
		return false
	}
	if host == localhostHost {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getDurationEnv(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		d, err := time.ParseDuration(value)
		if err != nil {
			return defaultValue
		}
		return d
	}
	return defaultValue
}

func getIntEnv(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return defaultValue
		}
		return n
	}
	return defaultValue
}

func getBoolEnv(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		b, err := strconv.ParseBool(value)
		if err != nil {
			return defaultValue
		}
		return b
	}
	return defaultValue
}
