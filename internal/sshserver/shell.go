/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshserver

import (
	"os"
	"os/exec"
	"os/user"
	"strings"
)

const (
	shellBash = "/bin/bash"
	shellSh   = "/bin/sh"

	// Account variables a login normally provides.
	envHome    = "HOME"
	envUser    = "USER"
	envLogname = "LOGNAME"
)

// buildShellCommand assembles the command for a session: the shell on its own
// for an interactive login, or the shell with -c for an exec request. The
// command always goes through a shell so that quoting, pipes and redirection in
// what the client sent behave the way the client expects.
func buildShellCommand(shell string, loginShell bool, rawCmd string) *exec.Cmd {
	if strings.TrimSpace(rawCmd) == "" {
		if loginShell {
			return exec.Command(shell, "-l")
		}
		return exec.Command(shell)
	}

	if loginShell {
		return exec.Command(shell, "-lc", rawCmd)
	}
	return exec.Command(shell, "-c", rawCmd)
}

// resolveShell picks the shell to run, preferring $SHELL and falling back to
// bash then sh. Workspace images vary, so the candidates are probed rather than
// assumed.
func resolveShell() string {
	candidates := []string{os.Getenv("SHELL"), shellBash, shellSh}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return shellSh
}

// mergeEnv combines the server process's environment with the variables the
// client sent, letting the client win on conflicts. The process environment is
// the base because it carries the image's own setup, such as PATH and the
// virtualenv, that the workspace needs.
func mergeEnv(sessionEnv []string) []string {
	merged := map[string]string{}
	for _, entry := range append(os.Environ(), sessionEnv...) {
		if key, value, ok := strings.Cut(entry, "="); ok {
			merged[key] = value
		}
	}

	out := make([]string, 0, len(merged))
	for key, value := range merged {
		out = append(out, key+"="+value)
	}
	return out
}

// withPasswdFallback fills the account variables from the passwd entry when the
// environment does not already carry them.
//
// The server's own environment is whatever its launcher gave it, and a launcher
// need not provide these: supervisord does not set HOME or USER for a program it
// starts under user=. Without HOME a session would run with no home directory at
// all, in the server's working directory, and IDEs install their server
// component under $HOME. OpenSSH resolves these from the passwd entry rather
// than trusting the daemon's environment, and defaultHostKeyPath already falls
// back the same way. Values already present are left alone, so a client can
// still override them.
func withPasswdFallback(env []string) []string {
	_, hasHome := lookupEnv(env, envHome)
	_, hasUser := lookupEnv(env, envUser)
	_, hasLogname := lookupEnv(env, envLogname)

	if hasHome && hasUser && hasLogname {
		return env
	}

	current, err := user.Current()
	if err != nil {
		return env
	}

	if !hasHome && current.HomeDir != "" {
		env = append(env, envHome+"="+current.HomeDir)
	}
	if current.Username != "" {
		if !hasUser {
			env = append(env, envUser+"="+current.Username)
		}
		if !hasLogname {
			env = append(env, envLogname+"="+current.Username)
		}
	}

	return env
}

// sessionWorkingDir returns the directory a session should start in, which is the
// home directory when it exists. An absent or unusable home is skipped rather
// than set, because a Dir that cannot be entered fails the command outright and
// a session in the wrong directory is far better than no session at all.
func sessionWorkingDir(env []string) string {
	home, ok := lookupEnv(env, envHome)
	if !ok || home == "" {
		return ""
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		return ""
	}
	return home
}

// lookupEnv reads a variable out of an environment slice, for the merged
// environment that has not been applied to the process.
func lookupEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}
