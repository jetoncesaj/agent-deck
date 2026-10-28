// Phase 4 tests for plugin auto-install. RFC: docs/rfc/PLUGIN_ATTACH.md §4.6.
//
// All real exec is stubbed via pluginInstallExec to avoid spawning
// claude binaries during unit tests.

package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type stubExecCall struct {
	name string
	args []string
}

// withStubPluginExec records every pluginInstallExec invocation and
// returns a controllable stub. The stub returns ("", nil) by default —
// individual tests override responseFn to simulate failure.
func withStubPluginExec(t *testing.T) (*[]stubExecCall, func(func(name string, args []string) ([]byte, error))) {
	t.Helper()
	var (
		mu       sync.Mutex
		recorded []stubExecCall
		respFn   = func(name string, args []string) ([]byte, error) { return nil, nil }
	)
	orig := pluginInstallExec
	pluginInstallExec = func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
		mu.Lock()
		recorded = append(recorded, stubExecCall{name: name, args: args})
		mu.Unlock()
		return respFn(name, args)
	}
	t.Cleanup(func() { pluginInstallExec = orig })
	setRespFn := func(fn func(name string, args []string) ([]byte, error)) { respFn = fn }
	return &recorded, setRespFn
}

// TestEnsurePluginsInstalled_NoOp_WhenNoPlugins asserts the function
// returns immediately without exec when Instance.Plugins is empty.
func TestEnsurePluginsInstalled_NoOp_WhenNoPlugins(t *testing.T) {
	calls, _ := withStubPluginExec(t)
	inst := &Instance{ID: "x", Tool: "claude"}
	if err := inst.EnsurePluginsInstalled("/fake/profile"); err != nil {
		t.Fatalf("EnsurePluginsInstalled: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("expected zero exec calls; got %d (%v)", len(*calls), *calls)
	}
}

// TestEnsurePluginsInstalled_HonoursAutoInstallOff asserts that a catalog
// plugin whose `auto_install` is false (or unset) is never installed by
// agent-deck: no `claude plugin` command runs against the account
// profile. The user installs it once by hand; the per-session scratch
// overlay still enables it where it is attached.
func TestEnsurePluginsInstalled_HonoursAutoInstallOff(t *testing.T) {
	withTempHome(t)
	writeConfig(t, os.Getenv("HOME"), `
[plugins.octopus]
name = "octopus"
source = "nyldn/claude-octopus"
auto_install = false

[plugins.discord]
name = "discord"
source = "claude-plugins-official"
`)
	calls, _ := withStubPluginExec(t)
	inst := &Instance{ID: "x", Tool: "claude", Plugins: []string{"octopus", "discord"}}
	if err := inst.EnsurePluginsInstalled(t.TempDir()); err != nil {
		t.Fatalf("EnsurePluginsInstalled: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("auto_install off must not run claude plugin commands; got %d calls (%v)", len(*calls), *calls)
	}
}

// TestEnsurePluginsInstalled_SkipsAlreadyInstalled asserts the
// idempotency check — when `<source>/plugins/<source>/<name>/` already
// exists, no exec runs.
func TestEnsurePluginsInstalled_SkipsAlreadyInstalled(t *testing.T) {
	home := withTempHome(t)
	writeConfig(t, home, `
[plugins.octopus]
name = "octopus"
source = "nyldn/claude-octopus"
auto_install = true
`)
	source := t.TempDir()
	// Pre-create the plugin dir matching claude's real layout:
	// <profile>/plugins/cache/<source>/<name>/<version>/
	pluginDir := filepath.Join(source, "plugins", "cache", "nyldn/claude-octopus", "octopus", "1.0.0")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	calls, _ := withStubPluginExec(t)
	inst := &Instance{ID: "x", Tool: "claude", Plugins: []string{"octopus"}}
	_ = inst.EnsurePluginsInstalled(source)
	if len(*calls) != 0 {
		t.Errorf("already-installed plugin must skip exec; got %d calls (%v)", len(*calls), *calls)
	}
}

// TestEnsurePluginsInstalled_RunsMarketplaceAddThenInstall asserts the
// happy-path order: `claude plugin marketplace add` then
// `claude plugin install <id>`.
func TestEnsurePluginsInstalled_RunsMarketplaceAddThenInstall(t *testing.T) {
	home := withTempHome(t)
	writeConfig(t, home, `
[plugins.octopus]
name = "octopus"
source = "nyldn/claude-octopus"
auto_install = true
`)
	source := t.TempDir() // empty source — plugin not installed
	calls, _ := withStubPluginExec(t)
	inst := &Instance{ID: "x", Tool: "claude", Plugins: []string{"octopus"}}
	_ = inst.EnsurePluginsInstalled(source)

	if len(*calls) != 2 {
		t.Fatalf("expected 2 exec calls (marketplace add, plugin install); got %d (%v)", len(*calls), *calls)
	}
	if (*calls)[0].name != "claude" || len((*calls)[0].args) < 4 || (*calls)[0].args[1] != "marketplace" {
		t.Errorf("call[0] should be `claude plugin marketplace add ...`; got %v", (*calls)[0])
	}
	if (*calls)[1].name != "claude" || (*calls)[1].args[1] != "install" {
		t.Errorf("call[1] should be `claude plugin install <id>`; got %v", (*calls)[1])
	}
	// Last arg of install MUST be the FQ id "<name>@<source>".
	last := (*calls)[1].args[len((*calls)[1].args)-1]
	if last != "octopus@nyldn/claude-octopus" {
		t.Errorf("install arg: got %q, want %q", last, "octopus@nyldn/claude-octopus")
	}
}

// TestEnsurePluginsInstalled_ContinuesOnInstallError asserts a single
// failed install does not block subsequent plugins (best-effort).
func TestEnsurePluginsInstalled_ContinuesOnInstallError(t *testing.T) {
	home := withTempHome(t)
	writeConfig(t, home, `
[plugins.octopus]
name = "octopus"
source = "nyldn/claude-octopus"
auto_install = true

[plugins.discord]
name = "discord"
source = "claude-plugins-official"
auto_install = true
`)
	source := t.TempDir()
	calls, setResp := withStubPluginExec(t)
	// Fail every "install" call, succeed marketplace adds.
	setResp(func(name string, args []string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "install" {
			return []byte("simulated install failure"), &simulatedExecErr{}
		}
		return nil, nil
	})

	inst := &Instance{ID: "x", Tool: "claude", Plugins: []string{"octopus", "discord"}}
	_ = inst.EnsurePluginsInstalled(source) // never errors by contract

	// Both plugins must have attempted both marketplace add + install
	// (4 calls total) despite the first install failing.
	if len(*calls) != 4 {
		t.Errorf("expected 4 exec calls (2 plugins × 2 commands); got %d (%v)", len(*calls), *calls)
	}
}

type simulatedExecErr struct{}

func (e *simulatedExecErr) Error() string { return "simulated install failure" }

// TestPluginLockPath_EquivalentSpellingsCollide asserts P4-2: lock key
// must hash on a canonicalized profile path so the SAME profile via two
// equivalent spellings (trailing slash, ".", "..") shares a lock.
// Otherwise same-profile concurrent installs race.
func TestPluginLockPath_EquivalentSpellingsCollide(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("HOME", home)
	def := &PluginDef{Name: "octopus", Source: "nyldn/claude-octopus"}

	cases := []struct {
		name string
		path string
	}{
		{"plain", "/Users/alice/.claude"},
		{"trailing-slash", "/Users/alice/.claude/"},
		{"redundant-segments", "/Users/alice/./.claude"},
		{"parent-then-back", "/Users/alice/foo/../.claude"},
	}

	var first string
	for i, tc := range cases {
		got, err := pluginLockPath(tc.path, def)
		if err != nil {
			t.Fatalf("[%s] pluginLockPath: %v", tc.name, err)
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("[%s] equivalent path %q produced different lock %q (first was %q) — same-profile race regression", tc.name, tc.path, got, first)
		}
	}
}

// TestIsSecretPluginEnv_BlocksKnownTokenPatterns asserts P4-1: any env
// key matching a credential suffix pattern is filtered even when its
// prefix is allow-listed (`npm_config_`, `BUN_`, `NODE_`, `NPM_`).
func TestIsSecretPluginEnv_BlocksKnownTokenPatterns(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		// Tokens — must block.
		{"NPM_TOKEN", true},
		{"NPM_AUTH_TOKEN", true},
		{"NODE_AUTH_TOKEN", true},
		{"BUN_AUTH_TOKEN", true},
		{"npm_config_authToken", true},
		{"npm_config__authToken", true},
		{"npm_config__auth", true},
		{"npm_config_password", true},
		{"npm_config_passwd", true},
		{"npm_config_username", true},
		{"npm_config_email", true},
		{"BUN_AUTH_USERNAME", true},
		{"NODE_API_KEY", true},
		{"GITHUB_TOKEN", true}, // not in allow-list anyway, but suffix would catch it
		// Non-secrets in allowed namespace — must NOT block.
		{"npm_config_registry", false},
		{"npm_config_proxy", false},
		{"npm_config_cache", false},
		{"NODE_OPTIONS", false},
		{"NODE_PATH", false},
		{"BUN_INSTALL", false},
		{"NPM_LOGLEVEL", false},
		{"NPM_PREFIX", false},
		{"PATH", false},
	}
	for _, tc := range cases {
		got := isSecretPluginEnv(tc.key)
		if got != tc.want {
			t.Errorf("isSecretPluginEnv(%q): got %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestScrubbedEnvForPluginInstall_DropsCredentialsInAllowedNamespace
// asserts the integration: even though `npm_config_*` is allow-listed
// by prefix, credentials matching the suffix blocklist are still dropped.
func TestScrubbedEnvForPluginInstall_DropsCredentialsInAllowedNamespace(t *testing.T) {
	t.Setenv("npm_config_registry", "https://example.com")
	t.Setenv("npm_config_authToken", "leak-me")
	t.Setenv("NODE_AUTH_TOKEN", "secret")
	t.Setenv("NODE_OPTIONS", "--max-old-space-size=4096")
	t.Setenv("PATH", "/usr/bin")

	scrubbed := scrubbedEnvForPluginInstall()
	got := map[string]string{}
	for _, kv := range scrubbed {
		eq := strings.IndexByte(kv, '=')
		if eq > 0 {
			got[kv[:eq]] = kv[eq+1:]
		}
	}
	// Allowed.
	for _, k := range []string{"npm_config_registry", "NODE_OPTIONS", "PATH"} {
		if _, ok := got[k]; !ok {
			t.Errorf("expected %q to pass scrub; got keys: %v", k, mapKeysSorted(got))
		}
	}
	// Blocked.
	for _, k := range []string{"npm_config_authToken", "NODE_AUTH_TOKEN"} {
		if _, leaked := got[k]; leaked {
			t.Errorf("credential %q leaked through scrub; got keys: %v", k, mapKeysSorted(got))
		}
	}
}

func mapKeysSorted(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPluginLockPath_IncludesProfileHashAndFlattensSourceSlash asserts
// the lock filename embeds an 8-char profile hash (P3-1 fix: per-profile
// lock scoping) AND flattens owner/repo slashes in source.
func TestPluginLockPath_IncludesProfileHashAndFlattensSourceSlash(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("HOME", home)
	def := &PluginDef{Name: "octopus", Source: "nyldn/claude-octopus"}

	pathA, err := pluginLockPath("/Users/alice/.claude", def)
	if err != nil {
		t.Fatalf("pluginLockPath A: %v", err)
	}
	pathB, err := pluginLockPath("/Users/alice/.claude-work", def)
	if err != nil {
		t.Fatalf("pluginLockPath B: %v", err)
	}

	// Different profiles → different lock paths (the whole point).
	if pathA == pathB {
		t.Errorf("lock paths for different profiles must differ; got %q == %q", pathA, pathB)
	}
	// Filename shape: plugin-<8hex>-<safeSource>-<name>.lock
	baseA := filepath.Base(pathA)
	if !strings.HasPrefix(baseA, "plugin-") || !strings.HasSuffix(baseA, "-nyldn--claude-octopus-octopus.lock") {
		t.Errorf("lock filename shape unexpected: %q", baseA)
	}
	// Same profile + same plugin → deterministic.
	pathA2, _ := pluginLockPath("/Users/alice/.claude", def)
	if pathA != pathA2 {
		t.Errorf("lock path must be deterministic for same (profile, def); got %q != %q", pathA, pathA2)
	}
}

// TestEnsurePluginsInstalled_LockHeldRetries asserts a held lock does
// NOT cause a panic — it's a best-effort path that logs and skips.
func TestEnsurePluginsInstalled_LockHeldRetries(t *testing.T) {
	home := withTempHome(t)
	writeConfig(t, home, `
[plugins.octopus]
name = "octopus"
source = "nyldn/claude-octopus"
auto_install = true
`)
	calls, _ := withStubPluginExec(t)

	// Stub the lock acquirer to always fail.
	orig := pluginLockAcquireFn
	pluginLockAcquireFn = func(path string) (func(), error) {
		return nil, &simulatedExecErr{}
	}
	t.Cleanup(func() { pluginLockAcquireFn = orig })

	inst := &Instance{ID: "x", Tool: "claude", Plugins: []string{"octopus"}}
	if err := inst.EnsurePluginsInstalled(t.TempDir()); err != nil {
		t.Fatalf("EnsurePluginsInstalled must never error; got %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("lock acquisition failure must short-circuit before exec; got %d calls", len(*calls))
	}
}

// stubClaudeSettingsWrites makes the exec stub behave like claude 2.1.x:
// `marketplace add` adds extraKnownMarketplaces and `install` adds
// enabledPlugins[<id>]=true to <profile>/settings.json (re-marshalled, so
// key order and formatting change too), and install drops the plugin code
// into the cache.
func stubClaudeSettingsWrites(t *testing.T, setResp func(func(name string, args []string) ([]byte, error)), profile string, installErr error) {
	t.Helper()
	setResp(func(name string, args []string) ([]byte, error) {
		path := filepath.Join(profile, "settings.json")
		settings := map[string]any{}
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &settings)
		}
		switch {
		case len(args) >= 3 && args[1] == "marketplace" && args[2] == "add":
			settings["extraKnownMarketplaces"] = map[string]any{args[len(args)-1]: map[string]any{}}
		case len(args) >= 2 && args[1] == "install":
			settings["enabledPlugins"] = map[string]any{args[len(args)-1]: true}
		}
		out, _ := json.Marshal(settings)
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatalf("stub write settings: %v", err)
		}
		if len(args) >= 2 && args[1] == "install" {
			if installErr != nil {
				return []byte("simulated install failure"), installErr
			}
			_ = os.MkdirAll(filepath.Join(profile, "plugins", "cache", "nyldn/claude-octopus", "octopus", "1.0.0"), 0o755)
		}
		return nil, nil
	})
}

// TestEnsurePluginsInstalled_AccountSettingsUnchanged asserts the fix for
// the global-enablement leak: `claude plugin install` writes
// enabledPlugins[<id>]=true into the account's settings.json, which would
// load the plugin in every session on that account. After an install the
// account's settings.json must be byte-for-byte what it was, with its
// mode, whether the install succeeded or failed, and an absent file must
// stay absent. The plugin code itself stays installed.
func TestEnsurePluginsInstalled_AccountSettingsUnchanged(t *testing.T) {
	const original = "{\n  \"model\": \"opus\",\n  \"theme\": \"dark\"\n}\n"
	tests := []struct {
		name       string
		existing   bool
		installErr error
	}{
		{name: "install succeeds", existing: true},
		{name: "install fails after writing", existing: true, installErr: &simulatedExecErr{}},
		{name: "no settings.json before", existing: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := withTempHome(t)
			writeConfig(t, home, `
[plugins.octopus]
name = "octopus"
source = "nyldn/claude-octopus"
auto_install = true
`)
			profile := t.TempDir()
			settingsPath := filepath.Join(profile, "settings.json")
			if tt.existing {
				if err := os.WriteFile(settingsPath, []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			calls, setResp := withStubPluginExec(t)
			stubClaudeSettingsWrites(t, setResp, profile, tt.installErr)

			inst := &Instance{ID: "x", Tool: "claude", Plugins: []string{"octopus"}}
			_ = inst.EnsurePluginsInstalled(profile)

			if len(*calls) != 2 {
				t.Fatalf("expected marketplace add + install; got %d calls (%v)", len(*calls), *calls)
			}
			if !tt.existing {
				if _, err := os.Lstat(settingsPath); !os.IsNotExist(err) {
					t.Fatalf("settings.json absent before install must stay absent; stat err=%v", err)
				}
				return
			}
			got, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Errorf("account settings.json changed by install:\ngot:  %q\nwant: %q", got, original)
			}
			info, err := os.Stat(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("settings.json mode: got %o, want 600", info.Mode().Perm())
			}
			if tt.installErr == nil && !pluginInstalled(profile, &PluginDef{Name: "octopus", Source: "nyldn/claude-octopus"}) {
				t.Errorf("plugin code must stay installed after settings restore")
			}
		})
	}
}

// TestEnsurePluginsInstalled_SymlinkedSettingsKeptAsLink asserts that a
// settings.json symlinked from a dotfiles checkout keeps its link and its
// target's bytes after an install.
func TestEnsurePluginsInstalled_SymlinkedSettingsKeptAsLink(t *testing.T) {
	const original = "{\n  \"theme\": \"dark\"\n}\n"
	home := withTempHome(t)
	writeConfig(t, home, `
[plugins.octopus]
name = "octopus"
source = "nyldn/claude-octopus"
auto_install = true
`)
	profile := t.TempDir()
	target := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(profile, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, setResp := withStubPluginExec(t)
	stubClaudeSettingsWrites(t, setResp, profile, nil)

	inst := &Instance{ID: "x", Tool: "claude", Plugins: []string{"octopus"}}
	_ = inst.EnsurePluginsInstalled(profile)

	if dest, err := os.Readlink(link); err != nil || dest != target {
		t.Fatalf("settings.json must stay a symlink to %q; got %q (err=%v)", target, dest, err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("symlink target changed by install:\ngot:  %q\nwant: %q", got, original)
	}
}

// TestEnsurePluginsInstalled_UnreadableSettingsSkipsInstall asserts that
// when the account's settings.json cannot be snapshotted, no install runs:
// agent-deck would have no way to undo claude's global enablement.
func TestEnsurePluginsInstalled_UnreadableSettingsSkipsInstall(t *testing.T) {
	home := withTempHome(t)
	writeConfig(t, home, `
[plugins.octopus]
name = "octopus"
source = "nyldn/claude-octopus"
auto_install = true
`)
	profile := t.TempDir()
	// A directory where the file should be cannot be read as settings.
	if err := os.Mkdir(filepath.Join(profile, "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	calls, _ := withStubPluginExec(t)
	inst := &Instance{ID: "x", Tool: "claude", Plugins: []string{"octopus"}}
	_ = inst.EnsurePluginsInstalled(profile)
	if len(*calls) != 0 {
		t.Errorf("unreadable account settings must skip the install; got %d calls (%v)", len(*calls), *calls)
	}
}
