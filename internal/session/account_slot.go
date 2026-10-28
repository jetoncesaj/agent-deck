package session

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// accountTool follows the same explicit provenance gate as the shell command
// builder. A regular shell command is not a tool-specific account operation.
func (i *Instance) accountTool() string {
	if i.Tool == "shell" && i.SubcommandPassthrough {
		if fields := strings.Fields(i.Command); len(fields) > 0 {
			return MatchTool(fields[0])
		}
	}
	return i.Tool
}

// ValidateAccount must run before registration or account-dependent loadout
// writes, as well as before starting an already registered session.
func (i *Instance) ValidateAccount() error {
	return ValidateAccountSlot(i.Account, i.accountTool())
}

// ValidateAccountSlot rejects a named slot that would otherwise fall through
// to ambient credentials. An empty slot preserves the existing default chain.
func ValidateAccountSlot(account, tool string) error {
	account = strings.TrimSpace(account)
	if account == "" {
		return nil
	}
	config, err := LoadUserConfig()
	if err != nil {
		return fmt.Errorf("load account configuration: %w", err)
	}
	if config == nil {
		return fmt.Errorf("account %q is not configured; run agent-deck accounts", account)
	}
	if _, exists := config.Profiles[account]; !exists {
		return fmt.Errorf("account %q is not configured; run agent-deck accounts", account)
	}
	if IsClaudeCompatible(tool) && config.GetProfileClaudeConfigDir(account) == "" {
		return fmt.Errorf("account %q has no Claude config_dir", account)
	}
	if IsCodexCompatible(tool) && config.GetProfileCodexConfigDir(account) == "" {
		return fmt.Errorf("account %q has no Codex config_dir", account)
	}
	if tool == "deepseek" && config.GetProfileDeepSeekConfigDir(account) == "" {
		return fmt.Errorf("account %q has no DeepSeek config_dir", account)
	}
	return nil
}

// DefaultAccountForNewSession returns the [claude].default_account slot a new
// Claude session starts on when nobody picked one (#2895), or "" to inherit
// the conductor/group/env chain. Callers apply it to Claude sessions only.
//
// It is a creation-time prefill, not a level of resolveClaudeConfigDir: the
// slot is stored on Instance.Account, so changing the default never moves an
// existing session (that stays switch-account's job). Instance.Account beats
// group and conductor config_dir, so the default yields to a
// [groups."<groupPath>".claude] (or ancestor group) config_dir and to a parent
// conductor's [conductors.<name>.claude] config_dir, which the user scoped
// more narrowly than one machine-wide switch. A slot no longer configured
// falls back to inherit with a log line instead of failing the create.
func DefaultAccountForNewSession(cfg *UserConfig, groupPath string, parent *Instance) string {
	if cfg == nil {
		return ""
	}
	account := strings.TrimSpace(cfg.Claude.DefaultAccount)
	if account == "" {
		return ""
	}
	if cfg.GetGroupClaudeConfigDir(groupPath) != "" {
		return ""
	}
	if parent != nil && parent.IsConductor && cfg.GetConductorClaudeConfigDir(parent.ResolveConductorNameThreadSafe()) != "" {
		return ""
	}
	if !slices.Contains(ConfiguredAccountNames(cfg), account) {
		sessionLog.Warn("default_account_not_configured", slog.String("account", account))
		return ""
	}
	return account
}

// AccountSlotIsDefault reports whether a session's stored account slot
// changes nothing about where it runs, so the session list can leave the
// account badge off ([display] hide_default_account_badge). That holds for
// no slot (inherit), for the [claude].default_account a new session in the
// group starts on (DefaultAccountForNewSession), and for a slot whose Claude
// config_dir is the directory the session's conductor, group, profile or
// global config resolves to without it. conductorName is the session's
// resolved conductor name ("" for none). CLAUDE_CONFIG_DIR is not consulted:
// the row speaks for config.toml, not for the shell the TUI started in.
func AccountSlotIsDefault(cfg *UserConfig, account, tool, groupPath, conductorName string, parent *Instance) bool {
	account = strings.TrimSpace(account)
	if account == "" {
		return true
	}
	if cfg == nil || !IsClaudeCompatible(tool) {
		return false
	}
	if account == DefaultAccountForNewSession(cfg, groupPath, parent) {
		return true
	}
	slotDir := cfg.GetProfileClaudeConfigDir(account)
	if slotDir == "" {
		return false
	}
	inherited := ""
	if conductorName != "" {
		inherited = cfg.GetConductorClaudeConfigDir(conductorName)
	}
	if inherited == "" {
		inherited = cfg.GetGroupClaudeConfigDir(groupPath)
	}
	if inherited == "" {
		inherited = cfg.GetProfileClaudeConfigDir(resolvedProcessProfile())
	}
	if inherited == "" && cfg.Claude.ConfigDir != "" {
		inherited = ExpandPath(cfg.Claude.ConfigDir)
	}
	if inherited == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		inherited = filepath.Join(home, ".claude")
	}
	return filepath.Clean(slotDir) == filepath.Clean(inherited)
}
