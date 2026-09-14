/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

const (
	hostKeyDirMode  = 0o700
	hostKeyFileMode = 0o600
)

// loadOrCreateHostKey returns the host key at path, generating and persisting
// one on first use, and reports whether it generated it. Persisting matters:
// with an ephemeral key the fingerprint changes on every restart and clients
// learn to disable host-key checking. A key that exists but cannot be parsed is
// an error rather than a cause to overwrite it.
func loadOrCreateHostKey(path string) (signer ssh.Signer, generated bool, err error) {
	pemBytes, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		parsed, parseErr := gossh.ParsePrivateKey(pemBytes)
		if parseErr != nil {
			return nil, false, fmt.Errorf("existing host key at %s is unreadable: %w", path, parseErr)
		}
		return parsed, false, nil
	case !errors.Is(readErr, os.ErrNotExist):
		return nil, false, fmt.Errorf("failed to read host key at %s: %w", path, readErr)
	}

	generatedPEM, err := generateHostKeyPEM()
	if err != nil {
		return nil, false, err
	}

	if mkErr := os.MkdirAll(filepath.Dir(path), hostKeyDirMode); mkErr != nil {
		return nil, false, fmt.Errorf("failed to create host key directory: %w", mkErr)
	}
	if writeErr := os.WriteFile(path, generatedPEM, hostKeyFileMode); writeErr != nil {
		return nil, false, fmt.Errorf("failed to write host key to %s: %w", path, writeErr)
	}

	parsed, err := gossh.ParsePrivateKey(generatedPEM)
	if err != nil {
		return nil, false, fmt.Errorf("failed to parse generated host key: %w", err)
	}
	return parsed, true, nil
}

// generateHostKeyPEM creates a new ed25519 host key as PKCS#8 PEM.
func generateHostKeyPEM() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate host key: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal host key: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
