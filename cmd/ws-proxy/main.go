/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

// Package main provides the entry point for the WebSocket proxy sidecar.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/go-logr/zapr"
	"github.com/jupyter-infra/workspace-websocket-proxy/internal/proxy"
	"github.com/jupyter-infra/workspace-websocket-proxy/internal/sshserver"
	"go.uber.org/zap"
)

// subcommandSSH selects the SSH server mode, which runs in the workspace
// container and terminates remote IDE sessions on loopback. The default mode is
// the WebSocket proxy that bridges traffic to it.
const subcommandSSH = "ssh"

func main() {
	if len(os.Args) > 1 && os.Args[1] == subcommandSSH {
		runSSHServer(os.Args[2:])
		return
	}

	// Health check mode for Kubernetes exec probes.
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":8080"
		}
		// Extract port from addr (handles ":8080", "0.0.0.0:8080", etc.)
		port := addr
		if idx := strings.LastIndex(addr, ":"); idx >= 0 {
			port = addr[idx:]
		}
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1%s/health", port))
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Setup logger
	zapLog, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create logger: %v\n", err)
		os.Exit(1)
	}
	logger := zapr.NewLogger(zapLog).WithName("ws-proxy")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Load configuration
	config, err := proxy.LoadConfig()
	if err != nil {
		logger.Error(err, "Invalid configuration")
		os.Exit(1)
	}

	// Create and start server
	server := proxy.NewServer(config, logger)

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// Wait for shutdown signal or server error
	select {
	case <-ctx.Done():
		logger.Info("Shutting down WebSocket proxy")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error(err, "Server shutdown failed")
			os.Exit(1)
		}
	case err := <-errCh:
		if err != nil {
			logger.Error(err, "Server exited with error")
			os.Exit(1)
		}
	}

	logger.Info("Server stopped")
}

// runSSHServer starts the SSH server that terminates remote IDE sessions.
// Flags override the environment so the same binary is usable by hand and from a
// supervisor config.
func runSSHServer(args []string) {
	zapLog, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create logger: %v\n", err)
		os.Exit(1)
	}
	logger := zapr.NewLogger(zapLog).WithName("ssh-server")

	fs := flag.NewFlagSet("ssh", flag.ExitOnError)
	port := fs.Int("port", 0, "port to listen on (loopback only); overrides SSH_LISTEN_ADDR")
	hostKey := fs.String("host-key", "", "path to the persisted host key; overrides SSH_HOST_KEY_PATH")
	loginShell := fs.Bool("login-shell", false, "run the session shell as a login shell; overrides SSH_LOGIN_SHELL")
	if err := fs.Parse(args); err != nil {
		logger.Error(err, "Failed to parse flags")
		os.Exit(1)
	}

	config := sshserver.LoadConfig()
	if *port != 0 {
		config.ListenAddr = fmt.Sprintf("127.0.0.1:%d", *port)
	}
	if *hostKey != "" {
		config.HostKeyPath = *hostKey
	}
	if isFlagSet(fs, "login-shell") {
		config.LoginShell = *loginShell
	}

	server, err := sshserver.New(config, logger)
	if err != nil {
		logger.Error(err, "Failed to create SSH server")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("Shutting down SSH server")
		if err := server.Shutdown(context.Background()); err != nil {
			logger.Error(err, "SSH server shutdown failed")
			os.Exit(1)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			logger.Error(err, "SSH server exited with error")
			os.Exit(1)
		}
	}

	logger.Info("SSH server stopped")
}

// isFlagSet reports whether the named flag was supplied on the command line,
// letting an explicit boolean flag override the environment while an omitted one
// leaves the environment value in place.
func isFlagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
