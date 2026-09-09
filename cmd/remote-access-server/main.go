/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

// Package main provides the entry point for the remote access server, the SSH
// server that runs inside the workspace container and terminates remote IDE
// sessions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gliderlabs/ssh"
	"github.com/go-logr/zapr"
	"github.com/jupyter-infra/workspace-websocket-proxy/internal/sshserver"
	"go.uber.org/zap"
)

func main() {
	zapLog, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create logger: %v\n", err)
		os.Exit(1)
	}
	logger := zapr.NewLogger(zapLog).WithName("remote-access-server")

	port := flag.Int("port", 0, "port to listen on (loopback only); overrides SSH_LISTEN_ADDR")
	hostKey := flag.String("host-key", "", "path to the persisted host key; overrides SSH_HOST_KEY_PATH")
	loginShell := flag.Bool("login-shell", false, "run the session shell as a login shell; overrides SSH_LOGIN_SHELL")
	flag.Parse()

	config := sshserver.LoadConfig()
	if *port != 0 {
		config.ListenAddr = fmt.Sprintf("127.0.0.1:%d", *port)
	}
	if *hostKey != "" {
		config.HostKeyPath = *hostKey
	}
	if isFlagSet("login-shell") {
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

func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
