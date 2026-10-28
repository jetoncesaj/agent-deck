// Plugin auto-install — shell-out to `claude plugin install <id>` when
// the plugin's catalog entry has auto_install = true and its code isn't
// yet on disk for this session's profile.
// RFC: docs/rfc/PLUGIN_ATTACH.md §4.6.
//
// Best-effort: every failure mode logs and returns nil; session start
// never blocks on install. If install fails, scratch settings.json
// still has enabledPlugins[<id>] = true and claude logs its own
// "plugin not found" at runtime.
//
// The install only puts plugin code on disk. `claude plugin marketplace
// add` and `claude plugin install` also write extraKnownMarketplaces and
// enabledPlugins[<id>] = true into the account's settings.json, which
// would load the plugin in every session on that account. The account's
// settings.json is snapshotted before the commands run and put back
// byte-for-byte afterwards; enablement lives only in the per-session
// scratch overlay (worker_scratch.go).

package session

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// pluginInstallExec is the test seam for spawning `claude plugin ...`.
// Env policy: only pass scrubbedEnvForPluginInstall + caller-specified
// env, so a malicious postinstall hook cannot exfiltrate inherited
// tokens like CLAUDE_API_KEY / TELEGRAM_BOT_TOKEN / GITHUB_TOKEN
// (RFC PLUGIN_ATTACH security finding G2).
var pluginInstallExec = func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(scrubbedEnvForPluginInstall(), env...)
	return cmd.CombinedOutput()
}

// Allow-list of host env vars passed through to `claude plugin ...`.
var pluginInstallEnvAllowList = map[string]struct{}{
	"PATH":            {},
	"HOME":            {},
	"USER":            {},
	"LOGNAME":         {},
	"SHELL":           {},
	"TMPDIR":          {},
	"TMP":             {},
	"TEMP":            {},
	"LANG":            {},
	"LC_ALL":          {},
	"LC_CTYPE":        {},
	"TERM":            {},
	"HTTP_PROXY":      {},
	"HTTPS_PROXY":     {},
	"NO_PROXY":        {},
	"http_proxy":      {},
	"https_proxy":     {},
	"no_proxy":        {},
	"SSH_AUTH_SOCK":   {}, // claude marketplace add may clone over SSH
	"GIT_SSH":         {},
	"GIT_SSH_COMMAND": {},
}

// Prefix allow-list for npm/bun/node families needed by postinstall hooks.
// Any key matching a prefix is still dropped if isSecretPluginEnv flags it
// (P4-1 finding: NPM_AUTH_TOKEN, npm_config__auth, etc.).
var pluginInstallEnvAllowPrefixes = []string{
	"npm_config_",
	"BUN_",
	"NODE_",
	"NPM_",
}

// Case-insensitive substrings that mark a key as a credential and force
// it dropped even when prefix-allowed.
var pluginInstallEnvSecretSuffixes = []string{
	"token",
	"password",
	"passwd",
	"_auth",
	"authtoken",
	"username",
	"email",
	"secret",
	"key",
	"credential",
	"apikey",
}

func isSecretPluginEnv(key string) bool {
	lower := strings.ToLower(key)
	for _, sfx := range pluginInstallEnvSecretSuffixes {
		if strings.Contains(lower, sfx) {
			return true
		}
	}
	return false
}

func scrubbedEnvForPluginInstall() []string {
	host := os.Environ() //nolint:forbidigo // strict allow-list scrub (CCD set explicitly by caller, TELEGRAM_* never allow-listed) — not the childenv chokepoint
	out := make([]string, 0, len(host))
	for _, kv := range host {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := kv[:eq]
		if _, ok := pluginInstallEnvAllowList[key]; ok {
			if !isSecretPluginEnv(key) {
				out = append(out, kv)
			}
			continue
		}
		for _, prefix := range pluginInstallEnvAllowPrefixes {
			if strings.HasPrefix(key, prefix) {
				if isSecretPluginEnv(key) {
					break
				}
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

const pluginInstallTimeout = 90 * time.Second

// EnsurePluginsInstalled shells out to `claude plugin install` for every
// catalog plugin in i.Plugins that has auto_install = true and whose code
// isn't already on disk under sourceProfileDir. A plugin with
// auto_install off is left for the user to install once; attaching it
// still enables it in the session's scratch settings. Best-effort: always
// returns nil. Concurrent installs across sessions serialize via a
// per-(profile, plugin) lock under `~/.agent-deck/locks/`.
func (i *Instance) EnsurePluginsInstalled(sourceProfileDir string) error {
	if i == nil || len(i.Plugins) == 0 || sourceProfileDir == "" {
		return nil
	}
	for _, name := range i.Plugins {
		def := GetPluginDef(name)
		if def == nil {
			continue
		}
		if pluginInstalled(sourceProfileDir, def) {
			continue
		}
		if !def.AutoInstall {
			sessionLog.Info("plugin_install_skipped",
				slog.String("instance_id", i.ID),
				slog.String("plugin_id", def.ID()),
				slog.String("reason", "auto_install_off"),
				slog.String("source_profile_dir", sourceProfileDir),
				slog.String("guidance", "plugin code is not installed in this profile; install it once, or set auto_install = true in its [plugins.<name>] catalog entry"),
			)
			continue
		}
		runPluginInstall(sourceProfileDir, def, i.ID)
	}
	return nil
}

// Claude layout: <sourceProfileDir>/plugins/cache/<source>/<name>/<version>/.
// Any version subdir counts as installed. <source> is the marketplace
// short name (e.g. "claude-plugins-official"), not "owner/repo".
func pluginInstalled(sourceProfileDir string, def *PluginDef) bool {
	if def == nil {
		return false
	}
	pluginDir := filepath.Join(sourceProfileDir, "plugins", "cache", def.Source, def.Name)
	entries, err := os.ReadDir(pluginDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return true
		}
	}
	return false
}

// runPluginInstall acquires the per-(profile, plugin) lock, registers
// the marketplace, then runs `claude plugin install`. Errors → logged
// warnings. The profile dir is part of the lock key so installs in
// different profiles don't serialize (P3-1 finding).
func runPluginInstall(sourceProfileDir string, def *PluginDef, instanceID string) {
	if def == nil {
		return
	}
	lockPath, err := pluginLockPath(sourceProfileDir, def)
	if err != nil {
		sessionLog.Warn("plugin_install_lock_path_failed",
			slog.String("instance_id", instanceID),
			slog.String("plugin_id", def.ID()),
			slog.String("error", err.Error()),
		)
		return
	}
	released, err := acquirePluginLock(lockPath)
	if err != nil {
		sessionLog.Warn("plugin_install_lock_failed",
			slog.String("instance_id", instanceID),
			slog.String("plugin_id", def.ID()),
			slog.String("error", err.Error()),
		)
		return
	}
	defer released()

	// Peer install may have completed while we waited on the lock.
	if pluginInstalled(sourceProfileDir, def) {
		return
	}

	// Refuse to run claude when its settings.json writes could not be
	// undone: the plugin would stay enabled for the whole account.
	snap, err := snapshotProfileSettings(sourceProfileDir)
	if err != nil {
		sessionLog.Warn("plugin_install_settings_snapshot_failed",
			slog.String("instance_id", instanceID),
			slog.String("plugin_id", def.ID()),
			slog.String("error", err.Error()),
		)
		return
	}
	defer func() {
		changed, err := snap.restore()
		if err != nil {
			sessionLog.Warn("plugin_install_settings_restore_failed",
				slog.String("instance_id", instanceID),
				slog.String("plugin_id", def.ID()),
				slog.String("settings_path", snap.path),
				slog.String("error", err.Error()),
			)
			return
		}
		if changed {
			sessionLog.Info("plugin_install_settings_restored",
				slog.String("instance_id", instanceID),
				slog.String("plugin_id", def.ID()),
				slog.String("settings_path", snap.path),
			)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), pluginInstallTimeout)
	defer cancel()

	env := []string{"CLAUDE_CONFIG_DIR=" + sourceProfileDir}

	// `marketplace add` exits non-zero on already-registered for some
	// claude versions; harmless, proceed to install.
	if out, err := pluginInstallExec(ctx, env, "claude", "plugin", "marketplace", "add", def.Source); err != nil {
		sessionLog.Info("plugin_install_marketplace_add_skipped",
			slog.String("instance_id", instanceID),
			slog.String("plugin_id", def.ID()),
			slog.String("output", string(out)),
		)
	}

	out, err := pluginInstallExec(ctx, env, "claude", "plugin", "install", def.ID())
	if err != nil {
		sessionLog.Warn("plugin_install_failed",
			slog.String("instance_id", instanceID),
			slog.String("plugin_id", def.ID()),
			slog.String("output", string(out)),
			slog.String("error", err.Error()),
		)
		return
	}
	sessionLog.Info("plugin_install_succeeded",
		slog.String("instance_id", instanceID),
		slog.String("plugin_id", def.ID()),
	)
}

// profileSettingsSnapshot is a profile's settings.json as it was before
// `claude plugin ...` ran. A symlinked settings.json (dotfiles) is read
// and restored through its target, and the link itself is put back if
// claude replaced it with a regular file.
type profileSettingsSnapshot struct {
	path     string // <profile>/settings.json
	target   string // file actually holding the bytes (path, or the symlink's target)
	linkDest string // raw symlink destination; "" when path was not a symlink
	existed  bool
	data     []byte
	perm     os.FileMode
}

func snapshotProfileSettings(profileDir string) (*profileSettingsSnapshot, error) {
	snap := &profileSettingsSnapshot{path: filepath.Join(profileDir, "settings.json")}
	snap.target = snap.path
	info, err := os.Lstat(snap.path)
	if os.IsNotExist(err) {
		return snap, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", snap.path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if snap.linkDest, err = os.Readlink(snap.path); err != nil {
			return nil, fmt.Errorf("reading symlink %s: %w", snap.path, err)
		}
		if snap.target, err = filepath.EvalSymlinks(snap.path); err != nil {
			return nil, fmt.Errorf("resolving symlink %s: %w", snap.path, err)
		}
		if info, err = os.Stat(snap.target); err != nil {
			return nil, fmt.Errorf("stat %s: %w", snap.target, err)
		}
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", snap.target)
	}
	if snap.data, err = os.ReadFile(snap.target); err != nil {
		return nil, fmt.Errorf("reading %s: %w", snap.target, err)
	}
	snap.existed = true
	snap.perm = info.Mode().Perm()
	return snap, nil
}

// restore puts settings.json back exactly as snapshotted and reports
// whether anything had to change.
func (s *profileSettingsSnapshot) restore() (bool, error) {
	if !s.existed {
		if _, err := os.Lstat(s.path); os.IsNotExist(err) {
			return false, nil
		}
		if err := os.Remove(s.path); err != nil {
			return false, fmt.Errorf("removing %s created by claude: %w", s.path, err)
		}
		return true, nil
	}
	changed := false
	if s.linkDest != "" {
		if dest, err := os.Readlink(s.path); err != nil || dest != s.linkDest {
			if err := symlinkReplace(s.linkDest, s.path); err != nil {
				return false, fmt.Errorf("restoring symlink %s: %w", s.path, err)
			}
			changed = true
		}
	}
	current, err := os.ReadFile(s.target)
	if err == nil && bytes.Equal(current, s.data) {
		if info, statErr := os.Stat(s.target); statErr == nil && info.Mode().Perm() == s.perm {
			return changed, nil
		}
	}
	if err := atomicWriteFile(s.target, s.data, s.perm); err != nil {
		return false, fmt.Errorf("restoring %s: %w", s.target, err)
	}
	return true, nil
}

// Lock identity = (canonical profile dir, source, name). Profile dir is
// hashed (FNV-1a) after Abs+Clean+EvalSymlinks so path-equivalent
// spellings collide on the same lock (P4-2 fix).
func pluginLockPath(sourceProfileDir string, def *PluginDef) (string, error) {
	if def == nil {
		return "", fmt.Errorf("nil PluginDef")
	}
	locks, err := dataPath("locks", "locks")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(locks, 0o700); err != nil {
		return "", err
	}
	safeSrc := pluginLockSafeName(def.Source)
	profileHash := fnv1aHex(canonicalProfileDir(sourceProfileDir))
	return filepath.Join(locks, fmt.Sprintf("plugin-%s-%s-%s.lock", profileHash, safeSrc, def.Name)), nil
}

// canonicalProfileDir normalizes profileDir for lock-key hashing.
// EvalSymlinks is best-effort (skipped when the path doesn't yet exist)
// so the function never returns "" for a non-empty input.
func canonicalProfileDir(profileDir string) string {
	if profileDir == "" {
		return ""
	}
	abs, err := filepath.Abs(profileDir)
	if err != nil {
		return filepath.Clean(profileDir)
	}
	clean := filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return real
	}
	return clean
}

// 8-char FNV-1a hex digest — enough to disambiguate typical profile counts
// without pulling in crypto.
func fnv1aHex(s string) string {
	const offset = 2166136261
	const prime = 16777619
	hash := uint32(offset)
	for i := 0; i < len(s); i++ {
		hash ^= uint32(s[i])
		hash *= prime
	}
	return fmt.Sprintf("%08x", hash)
}

// Replaces `/` in `<owner>/<repo>` source with `--`; other chars pass
// through (marketplace ids are URL-safe by convention).
func pluginLockSafeName(source string) string {
	out := make([]byte, 0, len(source))
	for i := 0; i < len(source); i++ {
		c := source[i]
		if c == '/' {
			out = append(out, '-', '-')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

// Test seam — tests stub this to simulate contention.
var pluginLockAcquireFn = defaultAcquirePluginLock

func acquirePluginLock(path string) (func(), error) {
	return pluginLockAcquireFn(path)
}

// O_CREATE|O_EXCL marker file with holder PID. Stale markers (dead
// process, or marker older than legacyStaleTTL — PID-reuse defense,
// P3-2) are reclaimed atomically.
const (
	pluginLockRetryInterval  = 250 * time.Millisecond
	pluginLockBudget         = 30 * time.Second
	pluginLockLegacyStaleTTL = 2 * time.Minute
)

func defaultAcquirePluginLock(path string) (func(), error) {
	deadline := time.Now().Add(pluginLockBudget)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}

		if reclaimStalePluginLock(path) {
			continue
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("plugin lock %q held by live process; gave up after %s", path, pluginLockBudget)
		}
		time.Sleep(pluginLockRetryInterval)
	}
}

func reclaimStalePluginLock(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	// Age trumps PID liveness: a marker older than legacyStaleTTL means
	// our install would have timed out anyway, so reclaim regardless of
	// what kill -0 says (PID-reuse defense, P3-2).
	if time.Since(info.ModTime()) > pluginLockLegacyStaleTTL {
		_ = os.Remove(path)
		return true
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	pid, parseErr := parsePluginLockPID(string(data))
	if parseErr != nil {
		return false
	}
	// kill -0 → ESRCH means no such process; safe to reclaim.
	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(path)
		return true
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		_ = os.Remove(path)
		return true
	}
	return false
}

func parsePluginLockPID(content string) (int, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return 0, fmt.Errorf("empty marker")
	}
	pid, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, err
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d", pid)
	}
	return pid, nil
}
