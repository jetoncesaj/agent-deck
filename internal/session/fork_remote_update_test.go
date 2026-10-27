package session

import (
	"context"
	"errors"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/update"
)

// A fork build (self-update off) never deploys an upstream release onto a
// remote: the release branch of DeployRemoteBinary refuses before probing
// the remote or fetching anything, and no binary is installed.
func TestForkBuild_DeployRemoteBinaryRefusesARelease(t *testing.T) {
	restore := update.SetSelfUpdateEnabledForTesting(false)
	defer restore()

	stub := &stubInstaller{platformOK: true}
	opts := stubReleaseOptions(map[string]*stubInstaller{"x": stub}, true)
	opts.NewRunner = nil
	_, err := DeployRemoteBinary(context.Background(), stub, "1.16.0", opts)
	if !errors.Is(err, update.ErrSelfUpdateDisabled) {
		t.Fatalf("DeployRemoteBinary err = %v, want ErrSelfUpdateDisabled", err)
	}
	if stub.installs != 0 {
		t.Fatalf("installs = %d, want 0", stub.installs)
	}
}
