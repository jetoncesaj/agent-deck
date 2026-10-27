package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// selfUpdateOnLDFlags builds a binary with upstream self-update behaviour
// (internal/update/fork.go); the shared CLI fixture uses it so the
// subprocess suite matches the in-process one (testmain_test.go).
const selfUpdateOnLDFlags = "-X github.com/asheshgoplani/agent-deck/internal/update.selfUpdate=on"

// A plain `go build` of the fork is a fork build: its version carries
// "-fork" and every `update` path that would replace the binary with an
// upstream release refuses before touching the network, the binary or any
// timer.
func TestForkBuild_BinaryNeverSelfUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a second CLI binary")
	}
	bin := filepath.Join(t.TempDir(), "agent-deck-fork")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	home := t.TempDir()

	stdout, stderr, code, err := execAgentDeckEnv(bin, home, "", nil, "version")
	if err != nil || code != 0 {
		t.Fatalf("version: exit %d, %v\nstdout: %s\nstderr: %s", code, err, stdout, stderr)
	}
	if !strings.Contains(stdout, "-fork") || strings.Contains(stdout, "update available") {
		t.Fatalf("version output %q: want the -fork suffix and no update notice", stdout)
	}

	for _, args := range [][]string{
		{"update"},
		{"update", "--check"},
		{"update", "--check", "--json"},
		{"update", "--version", "1.16.33"},
		{"update", "--unattended"},
		{"update", "--check-now"},
		{"update", "--install-timer", "--dry-run"},
		{"update", "--ensure-timer", "--dry-run"},
	} {
		stdout, stderr, code, err := execAgentDeckEnv(bin, home, "", nil, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if code == 0 {
			t.Errorf("%v succeeded in a fork build\nstdout: %s\nstderr: %s", args, stdout, stderr)
		}
		if !strings.Contains(stderr, "self-update is disabled in this fork build") {
			t.Errorf("%v: stderr lacks the fork refusal: %s", args, stderr)
		}
	}
}
