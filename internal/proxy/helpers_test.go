/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package proxy

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
)

func testLogger() logr.Logger {
	zapLog, _ := zap.NewDevelopment()
	return zapr.NewLogger(zapLog)
}

func testConfig() *Config {
	return &Config{
		ListenAddr:           ":0",
		TargetHost:           "127.0.0.1",
		TargetPort:           0,
		MaxSessionDuration:   5 * time.Second,
		PingInterval:         1 * time.Second,
		PingTimeout:          2 * time.Second,
		MaxConnections:       2,
		ReadLimit:            65536,
		TargetHealthInterval: 50 * time.Millisecond,
	}
}

var (
	connectionCountsMu sync.Mutex
	connectionCounts   = map[string]int{}
)

func startCountingTCPServer(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			connectionCountsMu.Lock()
			connectionCounts[addr]++
			connectionCountsMu.Unlock()

			_ = conn.Close()
		}
	}()

	return addr, func() { _ = listener.Close() }
}

func connectionCount(addr string) int {
	connectionCountsMu.Lock()
	defer connectionCountsMu.Unlock()
	return connectionCounts[addr]
}

func startBannerTCPServer(t *testing.T, banner string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := c.Write([]byte(banner)); err != nil {
					return
				}
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if _, err := c.Write(buf[:n]); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return listener.Addr().String(), func() { _ = listener.Close() }
}

func startWedgedTCPServer(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener.Addr().String(), func() { _ = listener.Close() }
}

func startEchoTCPServer(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if _, err := c.Write(buf[:n]); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return listener.Addr().String(), func() { _ = listener.Close() }
}
