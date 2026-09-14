/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package proxy

import (
	"net"
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
		ListenAddr:         ":0",
		TargetHost:         "127.0.0.1",
		TargetPort:         0,
		MaxSessionDuration: 5 * time.Second,
		PingInterval:       1 * time.Second,
		PingTimeout:        2 * time.Second,
		MaxConnections:     2,
		ReadLimit:          65536,
	}
}

// startBannerTCPServer starts a TCP server that writes banner on connect and
// then echoes, standing in for the remote access server's SSH greeting.
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

// startWedgedTCPServer starts a listener that never calls Accept, reproducing a
// target process that is deadlocked. The kernel still completes the TCP
// handshake from the listen backlog, so a dial succeeds and only a read
// detects it.
func startWedgedTCPServer(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener.Addr().String(), func() { _ = listener.Close() }
}

// startEchoTCPServer starts a TCP server that echoes received data back.
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
