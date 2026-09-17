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
	if writeErr := writeHostKeyAtomically(path, generatedPEM); writeErr != nil {
		return nil, false, writeErr
	}

	parsed, err := gossh.ParsePrivateKey(generatedPEM)
	if err != nil {
		return nil, false, fmt.Errorf("failed to parse generated host key: %w", err)
	}
	return parsed, true, nil
}

// writeHostKeyAtomically installs the key so path never holds a partial file,
// which loadOrCreateHostKey would refuse to parse and never repair. It writes a
// temporary file in the same directory, since rename is only atomic within one
// filesystem, and flushes before renaming so a crashed node cannot leave the
// rename durable with the contents still in the page cache.
func writeHostKeyAtomically(path string, contents []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create a temporary host key in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

	if err := writeAndSync(tmp, contents); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to write host key to %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to install host key at %s: %w", path, err)
	}

	return syncDir(dir)
}

// writeAndSync writes contents, restricts the mode and flushes to durable
// storage, closing the file on every path.
func writeAndSync(file *os.File, contents []byte) (err error) {
	defer func() {
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}()

	if err := file.Chmod(hostKeyFileMode); err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		return err
	}
	return file.Sync()
}

// syncDir flushes a directory entry. Failure is not fatal: the key is already
// written and usable, and some filesystems refuse the operation outright.
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer func() { _ = handle.Close() }()

	_ = handle.Sync()
	return nil
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
