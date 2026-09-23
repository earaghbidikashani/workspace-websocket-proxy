/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package proxy

import (
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if config.ListenAddr != ":8080" {
		t.Errorf("expected :8080, got %s", config.ListenAddr)
	}
	if config.TargetHost != "127.0.0.1" {
		t.Errorf("expected 127.0.0.1, got %s", config.TargetHost)
	}
	if config.TargetPort != 2222 {
		t.Errorf("expected 2222, got %d", config.TargetPort)
	}
	if config.MaxSessionDuration != 12*time.Hour {
		t.Errorf("expected 12h, got %s", config.MaxSessionDuration)
	}
	if config.MaxConnections != 10 {
		t.Errorf("expected 10, got %d", config.MaxConnections)
	}
	if config.ReadLimit != 65536 {
		t.Errorf("expected 65536, got %d", config.ReadLimit)
	}
	if config.TargetHealthBannerPrefix != "SSH-2.0-" {
		t.Errorf("expected SSH-2.0-, got %q", config.TargetHealthBannerPrefix)
	}
	if config.TargetHealthInterval != defaultTargetHealthInterval {
		t.Errorf("expected %s, got %s", defaultTargetHealthInterval, config.TargetHealthInterval)
	}
}

func TestConfigRejectsTargetHealthIntervalBelowProbeTimeout(t *testing.T) {
	t.Setenv("TARGET_HEALTH_INTERVAL", "1s")

	if _, err := LoadConfig(); err == nil {
		t.Error("expected an interval shorter than the probe timeout to be rejected")
	}
}

func TestConfigAcceptsTargetHealthInterval(t *testing.T) {
	t.Setenv("TARGET_HEALTH_INTERVAL", "5m")

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := 5 * time.Minute; config.TargetHealthInterval != want {
		t.Errorf("expected %s, got %s", want, config.TargetHealthInterval)
	}
}

func TestConfigBannerPrefixHonoursExplicitEmpty(t *testing.T) {
	t.Setenv("TARGET_HEALTH_BANNER_PREFIX", "")

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.TargetHealthBannerPrefix != "" {
		t.Errorf("expected the banner check to be disabled, got %q", config.TargetHealthBannerPrefix)
	}
}

func TestConfigTargetAddr(t *testing.T) {
	config := &Config{TargetHost: "localhost", TargetPort: 3333}
	expected := "localhost:3333"
	if config.TargetAddr() != expected {
		t.Errorf("expected %s, got %s", expected, config.TargetAddr())
	}
}

func TestConfigInvalidPort(t *testing.T) {
	t.Setenv("TARGET_PORT", "99999")
	_, err := LoadConfig()
	if err == nil {
		t.Error("expected error for invalid port")
	}
}

func TestConfigPingIntervalExceedsTimeout(t *testing.T) {
	t.Setenv("PING_INTERVAL", "60s")
	t.Setenv("PING_TIMEOUT", "30s")
	_, err := LoadConfig()
	if err == nil {
		t.Error("expected error when ping interval >= ping timeout")
	}
}

func TestConfigInvalidReadLimit(t *testing.T) {
	t.Setenv("READ_LIMIT", "-1")
	_, err := LoadConfig()
	if err == nil {
		t.Error("expected error for invalid read limit")
	}
}
