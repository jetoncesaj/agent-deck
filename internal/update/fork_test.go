package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// forkBuild runs the test as a fork build: self-update off, the default of
// a plain `go build` of this fork.
func forkBuild(t *testing.T) {
	t.Helper()
	t.Cleanup(SetSelfUpdateEnabledForTesting(false))
}

// selfUpdateAtInit is the switch as this test binary was built (no -X),
// captured before TestMain turns it on for the upstream suite.
var selfUpdateAtInit = selfUpdate

func TestForkBuild_SelfUpdateOffByDefault(t *testing.T) {
	if selfUpdateAtInit == selfUpdateOn {
		t.Fatal("a build without -X ...selfUpdate=on has self-update on")
	}
	forkBuild(t)
	if SelfUpdateEnabled() {
		t.Fatal("SelfUpdateEnabled() = true with the build flag unset")
	}
}

func TestForkBuild_VersionSuffixAndOrdering(t *testing.T) {
	forkBuild(t)
	for in, want := range map[string]string{
		"1.16.33":            "1.16.33-fork",
		"1.16.33-fork":       "1.16.33-fork",
		"1.16.33-3-gabc1234": "1.16.33-3-gabc1234-fork",
		"":                   "",
	} {
		if got := ForkVersion(in); got != want {
			t.Errorf("ForkVersion(%q) = %q, want %q", in, got, want)
		}
	}
	// The suffix is build metadata, not a pre-release: a fork build of
	// 1.16.33 is the same version as upstream 1.16.33, so upstream 1.16.33
	// never "supersedes" it, while a newer release still orders above it.
	if c := CompareVersions("1.16.33-fork", "1.16.33"); c != 0 {
		t.Errorf("CompareVersions(1.16.33-fork, 1.16.33) = %d, want 0", c)
	}
	if ReleaseSupersedes("1.16.33", "1.16.33-fork") {
		t.Error("upstream 1.16.33 supersedes the 1.16.33 fork build")
	}
	if !ReleaseSupersedes("1.16.34", "1.16.33-fork") {
		t.Error("1.16.34 should order above the 1.16.33 fork build")
	}
	if c := CompareVersions("1.16.4-rc.1", "1.16.4"); c != -1 {
		t.Errorf("a real pre-release must still sort below its release, got %d", c)
	}
	restore := SetSelfUpdateEnabledForTesting(true)
	if got := ForkVersion("1.16.33"); got != "1.16.33" {
		t.Errorf("ForkVersion with self-update on = %q, want unchanged", got)
	}
	restore()
}

// No update check, cache read, release lookup, changelog fetch or download
// reaches the network in a fork build.
func TestForkBuild_NoUpdateCheckOrDownload(t *testing.T) {
	forkBuild(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tag_name":"v99.0.0","assets":[]}`))
	}))
	defer srv.Close()
	origURL := apiBaseURL
	apiBaseURL = srv.URL
	t.Cleanup(func() { apiBaseURL = origURL })

	info, err := CheckForUpdate("1.16.33-fork", true)
	if err != nil || info == nil || info.Available {
		t.Fatalf("CheckForUpdate = %+v, %v; want no update and no error", info, err)
	}
	if cached, err := CachedUpdateInfo("1.16.33-fork"); cached != nil || err != nil {
		t.Fatalf("CachedUpdateInfo = %+v, %v; want nil", cached, err)
	}
	if ShouldNudge(&UpdateInfo{Available: true, ReleasesBehind: NudgeThreshold + 10}) {
		t.Fatal("ShouldNudge true in a fork build")
	}
	if _, err := FetchLatestRelease(); !errors.Is(err, ErrSelfUpdateDisabled) {
		t.Fatalf("FetchLatestRelease err = %v", err)
	}
	if _, err := FetchReleaseByTag("v1.16.33"); !errors.Is(err, ErrSelfUpdateDisabled) {
		t.Fatalf("FetchReleaseByTag err = %v", err)
	}
	if _, err := FetchChangelog(); !errors.Is(err, ErrSelfUpdateDisabled) {
		t.Fatalf("FetchChangelog err = %v", err)
	}
	rel := &Release{TagName: "v99.0.0"}
	if _, err := DownloadVerifiedBinaryContext(context.Background(), rel, "darwin", "arm64", nil); !errors.Is(err, ErrSelfUpdateDisabled) {
		t.Fatalf("DownloadVerifiedBinaryContext err = %v", err)
	}
	if err := PerformVerifiedUpdate(rel, "darwin", "arm64"); !errors.Is(err, ErrSelfUpdateDisabled) {
		t.Fatalf("PerformVerifiedUpdate err = %v", err)
	}
	if _, err := RunUnattendedInstall(context.Background(), "/nonexistent/agent-deck", "test"); !errors.Is(err, ErrSelfUpdateDisabled) {
		t.Fatalf("RunUnattendedInstall err = %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("fork build made %d GitHub API requests", n)
	}
}

// The com.agentdeck.autoupdate timer (and the systemd timer) is never
// planned or installed: the plan is a skip with no steps, and a plan made
// elsewhere is not executed.
func TestForkBuild_NeverInstallsTheUpdateTimer(t *testing.T) {
	forkBuild(t)
	home := t.TempDir()
	for _, goos := range []string{"darwin", "linux"} {
		c := TimerConfig{
			GOOS: goos, Home: home, Exe: "/usr/local/bin/agent-deck", UID: 501,
			LogDir: filepath.Join(home, "logs"), LaunchAgentsDir: filepath.Join(home, "LaunchAgents"),
			SystemdUserDir: filepath.Join(home, "systemd"),
		}
		p, err := PlanEnsureTimer(c, nil, false)
		if err != nil {
			t.Fatalf("%s: PlanEnsureTimer: %v", goos, err)
		}
		if len(p.Steps) != 0 || p.Result.Action != TimerActionSkipped {
			t.Fatalf("%s: plan = %+v, want a skip with no steps", goos, p)
		}
		res, err := RunEnsurePlan(c, nil, TimerEnsurePlan{Plan: Plan{Steps: []Step{{Desc: "must not run"}}}}, nil)
		if err != nil || res.Action != TimerActionSkipped {
			t.Fatalf("%s: RunEnsurePlan = %+v, %v; want a skip", goos, res, err)
		}
	}
}
