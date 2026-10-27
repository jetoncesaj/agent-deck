package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `session output <id>` returned another session's last message when several
// sessions shared one project cwd. Field shape: the target's own transcript
// lived under its group/account Claude config dir; the primary read resolved
// only the process-wide config dir, missed it, and the best-effort chain fell
// through to findLatestClaudeTranscriptOnDisk, which picked the NEWEST
// transcript in the shared project directory: a sibling's. The read must use
// the session's own bound transcript, and fail clearly when it is missing.

const (
	ownTranscriptSiblingReply = "SIBLING SESSION REPLY"
	ownTranscriptTargetReply  = "TARGET SESSION REPLY"
	ownTranscriptSiblingID    = "aaaaaaaa-0000-4000-8000-00000000000a"
	ownTranscriptTargetID     = "bbbbbbbb-0000-4000-8000-00000000000b"
)

type ownTranscriptFixture struct {
	home, projectPath, resolved string
	sibling                     *Instance
}

func setupOwnTranscript(t *testing.T) ownTranscriptFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	agentDeckDir := filepath.Join(home, ".agent-deck")
	if err := os.MkdirAll(agentDeckDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[groups.\"team-a\".claude]\nconfig_dir = \"~/.claude-team-a\"\n"
	if err := os.WriteFile(filepath.Join(agentDeckDir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)

	projectPath := filepath.Join(home, "shared-project")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved := projectPath
	if r, err := filepath.EvalSymlinks(projectPath); err == nil {
		resolved = r
	}
	sibling := NewInstance("sibling", projectPath)
	sibling.Tool = "claude"
	sibling.ClaudeSessionID = ownTranscriptSiblingID
	sibling.Status = StatusRunning
	fx := ownTranscriptFixture{home: home, projectPath: projectPath, resolved: resolved, sibling: sibling}
	// Sibling's transcript: process-wide config dir, newest by mtime.
	fx.writeTranscript(t, filepath.Join(home, ".claude"), ownTranscriptSiblingID, ownTranscriptSiblingReply, time.Now())
	return fx
}

func (fx ownTranscriptFixture) writeTranscript(t *testing.T, configDir, id, reply string, mod time.Time) {
	t.Helper()
	dir := filepath.Join(configDir, "projects", ConvertToClaudeDirName(fx.resolved))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(
		`{"type":"user","cwd":%q,"sessionId":%q,"message":{"role":"user","content":"hi"}}`+"\n"+
			`{"type":"assistant","cwd":%q,"sessionId":%q,"timestamp":"2026-10-27T10:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`+"\n",
		fx.resolved, id, fx.resolved, id, reply)
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func (fx ownTranscriptFixture) target(id string) *Instance {
	inst := NewInstance("target", fx.projectPath)
	inst.Tool = "claude"
	inst.GroupPath = "team-a"
	inst.ClaudeSessionID = id
	inst.Status = StatusRunning
	// Started before the sibling's last write, so the old disk-scan mtime
	// filter does not exclude the sibling.
	inst.LastStartedAt = time.Now().Add(-time.Hour)
	return inst
}

// readLikeSessionOutput mirrors handleSessionOutput's read.
func readLikeSessionOutput(inst *Instance, peers []*Instance) (*ResponseOutput, error) {
	version := inst.ResponseContentVersion(peers)
	resp, _, err := inst.GetLastResponseAtVersion(peers, version)
	return resp, err
}

func TestSessionOutput_ReadsOwnTranscriptInSharedCwd(t *testing.T) {
	fx := setupOwnTranscript(t)
	target := fx.target(ownTranscriptTargetID)
	// Target's own transcript: its group's config dir, OLDER than the sibling's.
	fx.writeTranscript(t, filepath.Join(fx.home, ".claude-team-a"), ownTranscriptTargetID, ownTranscriptTargetReply, time.Now().Add(-time.Minute))
	peers := []*Instance{fx.sibling, target}

	resp, err := readLikeSessionOutput(target, peers)
	if err != nil {
		t.Fatalf("session output read: %v", err)
	}
	if resp.Content != ownTranscriptTargetReply {
		t.Fatalf("session output = %q, want the target's own reply %q", resp.Content, ownTranscriptTargetReply)
	}
	if target.ClaudeSessionID != ownTranscriptTargetID {
		t.Fatalf("ClaudeSessionID mutated to %q", target.ClaudeSessionID)
	}
	// The best-effort chain (send --wait, TUI) resolves the same file now.
	if resp, err := target.GetLastResponseBestEffort(); err != nil || resp.Content != ownTranscriptTargetReply {
		t.Fatalf("GetLastResponseBestEffort = %+v, %v; want %q", resp, err, ownTranscriptTargetReply)
	}
}

func TestSessionOutput_MissingOwnTranscriptErrorsInsteadOfSibling(t *testing.T) {
	fx := setupOwnTranscript(t)
	target := fx.target(ownTranscriptTargetID) // no transcript anywhere
	peers := []*Instance{fx.sibling, target}

	resp, err := readLikeSessionOutput(target, peers)
	if err == nil {
		t.Fatalf("expected an error for a missing bound transcript, got %+v", resp)
	}
	if resp != nil && strings.Contains(resp.Content, ownTranscriptSiblingReply) {
		t.Fatalf("session output returned the sibling's reply")
	}
	if !strings.Contains(err.Error(), ownTranscriptTargetID) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error %q should name the bound claude_session_id and say not found", err)
	}
	if target.ClaudeSessionID != ownTranscriptTargetID {
		t.Fatalf("ClaudeSessionID mutated to %q; must never adopt a sibling's id", target.ClaudeSessionID)
	}
}

func TestSessionOutput_NoBoundIDErrors(t *testing.T) {
	fx := setupOwnTranscript(t)
	target := fx.target("")
	resp, err := readLikeSessionOutput(target, []*Instance{fx.sibling, target})
	if err == nil {
		t.Fatalf("expected an error for a session without a bound id, got %+v", resp)
	}
	if !strings.Contains(err.Error(), "no bound claude_session_id") {
		t.Fatalf("error %q should say no bound claude_session_id", err)
	}
}
