/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package proxy

import (
	"os/exec"
	"strings"
	"testing"
)

const (
	sidecarPackage      = "github.com/jupyter-infra/workspace-websocket-proxy/cmd/ws-proxy"
	remoteAccessPackage = "github.com/jupyter-infra/workspace-websocket-proxy/cmd/remote-access-server"
)

var sshOnlyDependencies = []string{
	"github.com/jupyter-infra/workspace-websocket-proxy/internal/sshserver",
	"github.com/gliderlabs/ssh",
	"github.com/creack/pty",
	"github.com/pkg/sftp",
}

func TestSidecarDoesNotDependOnTheSSHServer(t *testing.T) {
	deps := transitiveDeps(t, sidecarPackage)

	for _, dependency := range sshOnlyDependencies {
		for _, dep := range deps {
			if dep == dependency || strings.HasPrefix(dep, dependency+"/") {
				t.Errorf("%s imports %s, which belongs to the remote access server; "+
					"the sidecar image must not carry the SSH dependencies", sidecarPackage, dep)
			}
		}
	}
}

func TestRemoteAccessServerDependsOnTheSSHServer(t *testing.T) {
	deps := transitiveDeps(t, remoteAccessPackage)

	for _, dependency := range sshOnlyDependencies {
		found := false
		for _, dep := range deps {
			if dep == dependency || strings.HasPrefix(dep, dependency+"/") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s no longer depends on %s, so the sidecar assertion proves nothing",
				remoteAccessPackage, dependency)
		}
	}
}

func transitiveDeps(t *testing.T, pkg string) []string {
	t.Helper()

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go toolchain is not on PATH")
	}

	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("failed to list dependencies of %s: %v", pkg, err)
	}

	return strings.Fields(string(out))
}
