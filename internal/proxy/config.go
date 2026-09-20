/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package proxy

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// defaultTargetHealthBannerPrefix is the SSH identification string prefix from
// RFC 4253 section 4.2 and the target is the remote access server
const defaultTargetHealthBannerPrefix = "SSH-2.0-"

// defaultTargetHealthInterval is frequent enough for alerting to be timely and
// infrequent enough that the connections are negligible to the target.
const defaultTargetHealthInterval = 30 * time.Second

// Config holds all configuration for the WebSocket proxy.
type Config struct {
	// ListenAddr is the address the HTTP server listens on.
	ListenAddr string

	// TargetHost is the TCP host to proxy to.
	TargetHost string

	// TargetPort is the TCP port to proxy to.
	TargetPort int

	// MaxSessionDuration is the maximum lifetime of a single connection.
	// After this duration, the connection is closed regardless of activity.
	MaxSessionDuration time.Duration

	// PingInterval is how often to send WebSocket ping frames.
	PingInterval time.Duration

	// PingTimeout is how long to wait for a pong response before closing.
	PingTimeout time.Duration

	// MaxConnections is the maximum number of concurrent WebSocket connections.
	// New connections are rejected with 429 when at capacity.
	MaxConnections int

	// ReadLimit is the maximum size in bytes of incoming WebSocket messages.
	ReadLimit int64

	// RevalidationInterval is how often to re-validate the session (future use).
	RevalidationInterval time.Duration

	// RevalidationEndpoint is the URL to call for re-validation (future use).
	RevalidationEndpoint string

	// TargetHealthBannerPrefix is the greeting the probe expects on connect, empty to disable.
	TargetHealthBannerPrefix string

	// TargetHealthInterval is how often the background prober checks the target.
	TargetHealthInterval time.Duration
}

// LoadConfig reads configuration from environment variables with sensible defaults.
// Returns an error if any value is invalid.
func LoadConfig() (*Config, error) {
	config := &Config{
		ListenAddr:           getEnv("LISTEN_ADDR", ":8080"),
		TargetHost:           getEnv("TARGET_HOST", "127.0.0.1"),
		TargetPort:           getIntEnv("TARGET_PORT", 2222),
		MaxSessionDuration:   getDurationEnv("MAX_SESSION_DURATION", 12*time.Hour),
		PingInterval:         getDurationEnv("PING_INTERVAL", 30*time.Second),
		PingTimeout:          getDurationEnv("PING_TIMEOUT", 60*time.Second),
		MaxConnections:       getIntEnv("MAX_CONNECTIONS", 10),
		ReadLimit:            int64(getIntEnv("READ_LIMIT", 65536)),
		RevalidationInterval: getDurationEnv("REVALIDATION_INTERVAL", 5*time.Minute),
		RevalidationEndpoint: getEnv("REVALIDATION_ENDPOINT", ""),

		TargetHealthBannerPrefix: getEnvAllowEmpty(
			"TARGET_HEALTH_BANNER_PREFIX", defaultTargetHealthBannerPrefix),
		TargetHealthInterval: getDurationEnv(
			"TARGET_HEALTH_INTERVAL", defaultTargetHealthInterval),
	}

	if config.TargetPort < 1 || config.TargetPort > 65535 {
		return nil, fmt.Errorf("TARGET_PORT must be between 1 and 65535, got: %d", config.TargetPort)
	}

	if config.PingInterval >= config.PingTimeout {
		return nil, fmt.Errorf(
			"PING_INTERVAL (%s) must be less than PING_TIMEOUT (%s)",
			config.PingInterval, config.PingTimeout,
		)
	}

	if config.ReadLimit < 1024 || config.ReadLimit > 10*1024*1024 {
		return nil, fmt.Errorf("READ_LIMIT must be between 1024 and 10485760, got: %d", config.ReadLimit)
	}

	if config.TargetHealthInterval <= targetHealthDialTimeout {
		return nil, fmt.Errorf(
			"TARGET_HEALTH_INTERVAL (%s) must be greater than the probe timeout (%s)",
			config.TargetHealthInterval, targetHealthDialTimeout,
		)
	}

	return config, nil
}

// TargetAddr returns the full target address in host:port format.
func (c *Config) TargetAddr() string {
	return fmt.Sprintf("%s:%d", c.TargetHost, c.TargetPort)
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvAllowEmpty is getEnv for settings where an explicitly empty value is
// meaningful rather than absent.
func getEnvAllowEmpty(key, defaultValue string) string {
	if value, ok := os.LookupEnv(key); ok {
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
