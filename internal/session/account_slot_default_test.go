package session

import (
	"os"
	"path/filepath"
	"testing"
)

// AccountSlotIsDefault: a stored slot is "default" (the row may leave its
// badge off) when it is no slot, the [claude].default_account a new session
// there starts on, or a slot whose config_dir is what the session's
// conductor, group, profile or global config would pick without it.
func TestAccountSlotIsDefault(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	slots := map[string]ProfileSettings{
		"main": {Claude: ProfileClaudeSettings{ConfigDir: filepath.Join(home, ".claude")}},
		"team": {Claude: ProfileClaudeSettings{ConfigDir: "/tmp/claude-team"}},
		"side": {Claude: ProfileClaudeSettings{ConfigDir: "/tmp/claude-side"}},
	}
	teamGroup := map[string]GroupSettings{"crew": {Claude: GroupClaudeSettings{ConfigDir: "/tmp/claude-team"}}}

	tests := []struct {
		name      string
		cfg       *UserConfig
		account   string
		tool      string
		groupPath string
		conductor string
		want      bool
	}{
		{name: "no slot is inherited", cfg: &UserConfig{Profiles: slots}, account: "", tool: "claude", want: true},
		{name: "no slot without config", cfg: nil, account: "", tool: "claude", want: true},
		{name: "slot on the default dir", cfg: &UserConfig{Profiles: slots}, account: "main", tool: "claude", want: true},
		{name: "slot elsewhere", cfg: &UserConfig{Profiles: slots}, account: "team", tool: "claude", want: false},
		{name: "slot matching the group dir", cfg: &UserConfig{Profiles: slots, Groups: teamGroup}, account: "team", tool: "claude", groupPath: "crew", want: true},
		{name: "slot matching an ancestor group dir", cfg: &UserConfig{Profiles: slots, Groups: teamGroup}, account: "team", tool: "claude", groupPath: "crew/epic-1", want: true},
		{name: "default slot not matching the group dir", cfg: &UserConfig{Profiles: slots, Groups: teamGroup}, account: "main", tool: "claude", groupPath: "crew", want: false},
		{name: "default_account", cfg: &UserConfig{Profiles: slots, Claude: ClaudeSettings{DefaultAccount: "side"}}, account: "side", tool: "claude", want: true},
		{name: "global config_dir", cfg: &UserConfig{Profiles: slots, Claude: ClaudeSettings{ConfigDir: "/tmp/claude-side"}}, account: "side", tool: "claude", want: true},
		{
			name:      "conductor dir",
			cfg:       &UserConfig{Profiles: slots, Conductors: map[string]ConductorOverrides{"lead": {Claude: ConductorClaudeSettings{ConfigDir: "/tmp/claude-side"}}}},
			account:   "side",
			tool:      "claude",
			conductor: "lead",
			want:      true,
		},
		{name: "unconfigured slot", cfg: &UserConfig{Profiles: slots}, account: "gone", tool: "claude", want: false},
		{name: "non-Claude tool keeps its slot", cfg: &UserConfig{Profiles: slots}, account: "main", tool: "codex", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AccountSlotIsDefault(tt.cfg, tt.account, tt.tool, tt.groupPath, tt.conductor, nil); got != tt.want {
				t.Fatalf("AccountSlotIsDefault(%q in %q) = %v, want %v", tt.account, tt.groupPath, got, tt.want)
			}
		})
	}
}
