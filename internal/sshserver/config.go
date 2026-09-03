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
	"time"
)

const (
	defaultPort = 2222

	defaultMaxSessions = 10

	hostKeyDirName = ".jupyter-k8s"

	hostKeyFileName = "ssh_host_ed25519_key"

	localhostHost = "localhost"
)

var ephemeralPathPrefixes = []string{"/tmp/", "/var/tmp/", "/run/", "/dev/shm/"}

// Config holds the SSH server configuration.
type Config struct {
	ListenAddr string

	HostKeyPath string

	IdleTimeout time.Duration

	MaxSessions int

	LoginShell bool

	AllowNonLoopback bool
}

// LoadConfig reads the SSH server configuration from the environment.
func LoadConfig() *Config {
	return &Config{
		ListenAddr:       getEnv("SSH_LISTEN_ADDR", fmt.Sprintf("127.0.0.1:%d", defaultPort)),
		HostKeyPath:      getEnv("SSH_HOST_KEY_PATH", defaultHostKeyPath()),
		IdleTimeout:      getDurationEnv("SSH_IDLE_TIMEOUT", 0),
		MaxSessions:      getIntEnv("SSH_MAX_SESSIONS", defaultMaxSessions),
		LoginShell:       getBoolEnv("SSH_LOGIN_SHELL", false),
		AllowNonLoopback: getBoolEnv("SSH_ALLOW_NON_LOOPBACK", false),
	}
}

func defaultHostKeyPath() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, hostKeyDirName, hostKeyFileName)
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if current, err := user.Current(); err == nil {
		return current.HomeDir
	}
	return ""
}

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

func (c *Config) hasEphemeralHostKeyPath() bool {
	for _, prefix := range ephemeralPathPrefixes {
		if strings.HasPrefix(c.HostKeyPath, prefix) {
			return true
		}
	}
	return false
}

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
