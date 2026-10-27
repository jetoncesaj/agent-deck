package update

import (
	"errors"
	"strings"
)

// selfUpdate is the fork's one build-time switch for every path by which the
// binary replaces itself (or a remote's binary) with an upstream GitHub
// release: the update check and notice, `update` self-install, the
// com.agentdeck.autoupdate timer, the unattended installers and the remote
// auto-update and nudges. It is off unless the build sets it:
//
//	go build -ldflags "-X github.com/asheshgoplani/agent-deck/internal/update.selfUpdate=on"
//
// A fork build is rebased onto upstream tags deliberately, never replaced by
// an upstream release behind the operator's back.
var selfUpdate = ""

// selfUpdateOn is the value of selfUpdate that restores upstream behaviour.
const selfUpdateOn = "on"

// ForkVersionSuffix is appended to the version string of a build whose
// self-update is off, so `version`, logs and remotes can tell a fork binary
// from the upstream release of the same number. CompareVersions treats it as
// build metadata: "1.16.33-fork" orders equal to "1.16.33", not below it.
const ForkVersionSuffix = "-fork"

// ErrSelfUpdateDisabled is returned by every self-update path in a fork
// build (selfUpdate off).
var ErrSelfUpdateDisabled = errors.New("self-update is disabled in this fork build (rebuild with -ldflags \"-X github.com/asheshgoplani/agent-deck/internal/update.selfUpdate=on\" to enable)")

// SelfUpdateEnabled reports whether this build may check for, download or
// install an upstream release (locally or on a remote).
func SelfUpdateEnabled() bool {
	return selfUpdate == selfUpdateOn
}

// SetSelfUpdateEnabledForTesting flips the build-time switch for a test and
// returns the restore func. Tests that exercise the upstream update flows
// turn it on; tests of the fork's default turn it off.
func SetSelfUpdateEnabledForTesting(on bool) (restore func()) {
	prev := selfUpdate
	if on {
		selfUpdate = selfUpdateOn
	} else {
		selfUpdate = ""
	}
	return func() { selfUpdate = prev }
}

// ForkVersion returns v with ForkVersionSuffix when self-update is off and v
// does not carry it yet; v unchanged otherwise.
func ForkVersion(v string) string {
	if SelfUpdateEnabled() || v == "" || hasForkSuffix(v) {
		return v
	}
	return v + ForkVersionSuffix
}

// hasForkSuffix reports whether v (without build metadata) ends in
// ForkVersionSuffix.
func hasForkSuffix(v string) bool {
	v, _, _ = strings.Cut(v, "+")
	return strings.HasSuffix(v, ForkVersionSuffix)
}
