/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshserver

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// defaultPort is the loopback port the proxy sidecar dials by default.
	defaultPort = 2222

	defaultMaxSessions = 10

	// defaultIdleTimeout reclaims a connection that has gone quiet. It is
	// deliberately generous, and matches the proxy's MAX_SESSION_DURATION
	// default: the proxy tears the WebSocket down at twelve hours, so an SSH
	// connection cannot usefully outlive that anyway.
	defaultIdleTimeout = 12 * time.Hour

	loopbackIPv4 = "127.0.0.1"

	// hostKeyDirName keeps the key where a person would look for an SSH key, and
	// clear of a glob over the workspace's Jupyter dotfiles, which cleanup scripts
	// delete.
	hostKeyDirName = ".ssh"

	hostKeyFileName = "ssh_host_ed25519_key"

	localhostHost = "localhost"

	rootPath = "/"
)

// ephemeralPathPrefixes are locations a restart wipes despite sitting on their
// own filesystem, which the root-device check therefore misses.
var ephemeralPathPrefixes = []string{"/tmp/", "/var/tmp/", "/run/", "/dev/shm/"}

// Config holds the SSH server configuration.
type Config struct {
	// ListenAddr is the address the SSH server listens on. It must be a
	// loopback address unless AllowNonLoopback is set.
	ListenAddr string

	// HostKeyPath is where the ed25519 host key is persisted. It should be on
	// storage that survives a container restart, otherwise the fingerprint
	// changes every time and clients learn to disable host-key checking.
	HostKeyPath string

	// IdleTimeout closes a connection after this long without I/O in either
	// direction. Zero disables it, which is not advisable: it is the only
	// backstop for a connection that never tears down cleanly. Expiry cancels
	// the session context, which is what terminates the session's processes.
	IdleTimeout time.Duration

	// MaxSessions caps concurrent shell and exec channels. Zero or negative
	// disables the cap.
	//
	// The cap covers shell and exec channels only. SFTP subsystems and port
	// forwards do not claim a slot, so concurrent transfers and forwarded
	// connections are unbounded. That is acceptable for a single-tenant
	// workspace pod but is worth knowing before relying on the number.
	MaxSessions int

	// LoginShell runs the session shell with -l, so it reads the user's login
	// profile.
	LoginShell bool

	// AllowNonLoopback lifts the loopback restriction on ListenAddr. Only set
	// it when an authentication layer equivalent to the ingress is provably in
	// front of the server.
	AllowNonLoopback bool
}

// LoadConfig reads the SSH server configuration from the environment.
func LoadConfig() *Config {
	return &Config{
		ListenAddr:       getEnv("SSH_LISTEN_ADDR", fmt.Sprintf("%s:%d", loopbackIPv4, defaultPort)),
		HostKeyPath:      getEnv("SSH_HOST_KEY_PATH", defaultHostKeyPath()),
		IdleTimeout:      getDurationEnv("SSH_IDLE_TIMEOUT", defaultIdleTimeout),
		MaxSessions:      getIntEnv("SSH_MAX_SESSIONS", defaultMaxSessions),
		LoginShell:       getBoolEnv("SSH_LOGIN_SHELL", false),
		AllowNonLoopback: getBoolEnv("SSH_ALLOW_NON_LOOPBACK", false),
	}
}

// defaultHostKeyPath places the host key under the user's home directory, which
// in a workspace is a persistent volume. It returns an empty string when no home
// directory can be determined, which validate turns into an actionable error.
func defaultHostKeyPath() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, hostKeyDirName, hostKeyFileName)
}

// homeDir resolves the user's home directory, falling back to the passwd entry
// when HOME is unset. os.UserHomeDir consults only HOME on Unix, and a process
// started by a supervisor may not inherit it.
func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if current, err := user.Current(); err == nil {
		return current.HomeDir
	}
	return ""
}

// validate rejects a configuration the server must not run with. The loopback
// check is the important one: this server performs no SSH authentication, so a
// non-loopback bind would expose an unauthenticated shell to the cluster
// network.
func (c *Config) validate() error {
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

// hasEphemeralHostKeyPath reports whether the host key lives somewhere a restart
// wipes, so the server can warn at startup. The root filesystem is ephemeral by
// definition, which catches a workspace whose volume is mounted elsewhere or
// absent; the prefix list still covers temporary filesystems, which have devices
// of their own.
func (c *Config) hasEphemeralHostKeyPath() bool {
	for _, prefix := range ephemeralPathPrefixes {
		if strings.HasPrefix(c.HostKeyPath, prefix) {
			return true
		}
	}
	return isOnRootDevice(filepath.Dir(c.HostKeyPath))
}

// isOnRootDevice reports whether path shares a filesystem with the root
// directory, resolving through the nearest existing ancestor because the key's
// directory is created after this runs.
func isOnRootDevice(path string) bool {
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		return false
	}

	pathInfo, ok := statNearestAncestor(path)
	if !ok {
		return false
	}

	return onSameDevice(pathInfo, rootInfo)
}

// statNearestAncestor walks up from path until it finds something that exists.
func statNearestAncestor(path string) (os.FileInfo, bool) {
	for {
		if info, err := os.Stat(path); err == nil {
			return info, true
		}

		parent := filepath.Dir(path)
		if parent == path {
			return nil, false
		}
		path = parent
	}
}

// onSameDevice compares the device identifiers directly rather than widening
// them, because the field is uint64 on Linux and int32 on Darwin, so any
// conversion is redundant on one of them.
func onSameDevice(a, b os.FileInfo) bool {
	aStat, aOK := a.Sys().(*syscall.Stat_t)
	bStat, bOK := b.Sys().(*syscall.Stat_t)
	if !aOK || !bOK {
		return false
	}
	return aStat.Dev == bStat.Dev
}

// isLoopback reports whether host names only the loopback interface. An empty
// host and "*" are rejected because both mean every interface when passed to
// net.Listen.
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

// The getEnv helpers below all fall back to the default when the variable is
// unset or empty, and the typed ones do the same when the value fails to parse,
// so a malformed override degrades to the default rather than failing startup.

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
