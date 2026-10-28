package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// One selector serves launch, add and the capacity pre-check. Sub-sessions
// may be parents. With [launch] nest_under_parent on (nestOn), a session
// picked up automatically from a sub-session lands under that sub-session's
// parent (the old one-hop flattening, now asked for). Off (nestOff, the
// default), it lands under the calling sub-session itself, unless the caller
// is at [launch] max_parent_depth.

var nestOn, nestOff = launchParentRule{NestUnderParent: true}, launchParentRule{}

type parentFixture struct {
	parent, child, other *session.Instance
	all                  []*session.Instance
}

func newParentFixture() parentFixture {
	parent := session.NewInstanceWithGroupAndTool("Parent", "/proj/parent", "parent-group", "claude")
	parent.ID = "par-0001"
	child := session.NewInstanceWithGroupAndTool("Child", "/proj/child", "child-group", "claude")
	child.ID = "chd-0001"
	child.SetParentWithPath(parent.ID, parent.ProjectPath)
	other := session.NewInstanceWithGroupAndTool("Other", "/proj/other", "other-group", "claude")
	other.ID = "oth-0001"
	return parentFixture{parent: parent, child: child, other: other, all: []*session.Instance{parent, child, other}}
}

func setCaller(t *testing.T, id string) {
	t.Helper()
	t.Setenv("AGENT_DECK_SESSION_ID", "")
	t.Setenv("AGENTDECK_INSTANCE_ID", id)
	t.Setenv("TMUX", "")
}

func lpErr(t *testing.T, err error) *launchParentError {
	t.Helper()
	var lpe *launchParentError
	if !errors.As(err, &lpe) {
		t.Fatalf("error %v (%T) is not a *launchParentError", err, err)
	}
	return lpe
}

func TestLaunchParent_TopLevelCallerBecomesParent(t *testing.T) {
	f := newParentFixture()
	setCaller(t, f.parent.ID)
	p, _, note, err := selectLaunchParent("", false, nestOn, f.all)
	if err != nil || p != f.parent || note != "" {
		t.Fatalf("got parent=%v note=%q err=%v, want the parent, no note", p, note, err)
	}
}

func TestLaunchParent_SubsessionCallerAttachesToItsParent(t *testing.T) {
	f := newParentFixture()
	setCaller(t, f.child.ID)
	p, by, note, err := selectLaunchParent("", false, nestOn, f.all)
	if err != nil || p != f.parent {
		t.Fatalf("got parent=%v err=%v, want the parent", p, err)
	}
	if by != f.child {
		t.Fatalf("launched-by %s, want the calling sub-session", titleOf(by))
	}
	if p.ProjectPath != "/proj/parent" {
		t.Fatalf("parent project path %q, want the parent's", p.ProjectPath)
	}
	if !strings.Contains(note, f.child.Title) || !strings.Contains(note, f.parent.Title) {
		t.Fatalf("note %q must name both titles", note)
	}
}

// A sub-session named with --parent is a parent like any other: the new
// session becomes its child, whatever nest_under_parent says (that key is
// only about the automatic parent).
func TestLaunchParent_ExplicitSubsessionParentIsAccepted(t *testing.T) {
	f := newParentFixture()
	for _, nest := range []launchParentRule{nestOff, nestOn} {
		for _, caller := range []string{"", f.child.ID, f.parent.ID} {
			setCaller(t, caller)
			for _, explicit := range []string{f.child.Title, f.child.ID} {
				p, by, note, err := selectLaunchParent(explicit, false, nest, f.all)
				if err != nil || p != f.child || by != nil || note != "" {
					t.Fatalf("nest=%v explicit %q from %q: got parent=%s by=%s note=%q err=%v, want the sub-session and nothing else", nest, explicit, caller, titleOf(p), titleOf(by), note, err)
				}
			}
		}
	}
}

// deepChain adds c2 under the fixture's child and c3 under c2: depths
// parent 0, child 1, c2 2, c3 3.
func deepChain(f parentFixture) (c2, c3 *session.Instance, all []*session.Instance) {
	c2 = session.NewInstanceWithGroupAndTool("C2", "/proj/c2", "g", "claude")
	c2.ID = "c2-00001"
	c2.SetParentWithPath(f.child.ID, f.child.ProjectPath)
	c3 = session.NewInstanceWithGroupAndTool("C3", "/proj/c3", "g", "claude")
	c3.ID = "c3-00001"
	c3.SetParentWithPath(c2.ID, c2.ProjectPath)
	return c2, c3, append(append([]*session.Instance{}, f.all...), c2, c3)
}

// An explicit parent already at the depth cap is refused with the depth
// error, not silently replaced: the user named that parent.
func TestLaunchParent_ExplicitParentAtTheCapIsRefused(t *testing.T) {
	f := newParentFixture()
	_, c3, all := deepChain(f)
	setCaller(t, "")
	p, _, _, err := selectLaunchParent(c3.ID, false, nestOff, all)
	if p != nil || err == nil {
		t.Fatalf("explicit parent at depth %d: got parent=%s err=%v, want the depth error", session.DefaultMaxParentDepth, titleOf(p), err)
	}
	lpe := lpErr(t, err)
	if lpe.Code != ErrCodeInvalidOperation || !strings.Contains(lpe.Message, "max_parent_depth = 3") || !strings.Contains(lpe.Message, c3.Title) {
		t.Fatalf("got %+v, want an invalid-operation error naming the parent and the cap", lpe)
	}
	// A lower cap refuses a shallower parent the same way; a higher one
	// accepts c3.
	if _, _, _, err := selectLaunchParent(f.child.ID, false, launchParentRule{MaxDepth: 1}, all); err == nil || !strings.Contains(err.Error(), "max_parent_depth = 1") {
		t.Fatalf("max_parent_depth = 1 under a sub-session: err=%v, want the depth error", err)
	}
	if p, _, _, err := selectLaunchParent(c3.ID, false, launchParentRule{MaxDepth: 4}, all); err != nil || p != c3 {
		t.Fatalf("max_parent_depth = 4: parent=%s err=%v, want c3", titleOf(p), err)
	}
}

// An automatic parent at the cap is replaced by the nearest ancestor with
// room, with a note and launched-by naming the caller.
func TestLaunchParent_AutomaticParentAtTheCapFlattens(t *testing.T) {
	f := newParentFixture()
	c2, c3, all := deepChain(f)
	for _, tc := range []struct {
		name     string
		caller   *session.Instance
		rule     launchParentRule
		want     *session.Instance
		noteBits []string
	}{
		{"default cap, caller at depth 3", c3, nestOff, c2, []string{"C3 is at the depth limit", "max_parent_depth = 3", "linked under its parent C2"}},
		{"cap 1, caller at depth 1", f.child, launchParentRule{MaxDepth: 1}, f.parent, []string{"Child is at the depth limit", "max_parent_depth = 1", "linked under its parent Parent"}},
		{"cap 2 with nest on, caller at depth 2", c2, launchParentRule{NestUnderParent: true, MaxDepth: 2}, f.child, []string{"C2 is at the depth limit", "linked under its parent Child"}},
		{"cap lowered below the caller", c3, launchParentRule{MaxDepth: 2}, f.child, []string{"C3 and its parent C2 are at the depth limit", "linked under their ancestor Child"}},
		{"nest on below the cap", c2, nestOn, f.child, []string{"parent C2 is a sub-session; linked under its parent Child"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCaller(t, tc.caller.ID)
			p, by, note, err := selectLaunchParent("", false, tc.rule, all)
			if err != nil || p != tc.want || by != tc.caller {
				t.Fatalf("got parent=%s by=%s err=%v, want parent=%s by=%s", titleOf(p), titleOf(by), err, titleOf(tc.want), tc.caller.Title)
			}
			for _, bit := range tc.noteBits {
				if !strings.Contains(note, bit) {
					t.Fatalf("note %q lacks %q", note, bit)
				}
			}
		})
	}
}

func TestLaunchParent_ExplicitTopLevelParentUnchanged(t *testing.T) {
	f := newParentFixture()
	setCaller(t, f.child.ID)
	p, _, note, err := selectLaunchParent(f.parent.Title, false, nestOn, f.all)
	if err != nil || p != f.parent || note != "" {
		t.Fatalf("got parent=%v note=%q err=%v, want the parent, no note", p, note, err)
	}
}

func TestLaunchParent_ExplicitParentUnknownErrors(t *testing.T) {
	f := newParentFixture()
	setCaller(t, "")
	p, _, _, err := selectLaunchParent("no-such-session", false, nestOn, f.all)
	if p != nil || err == nil {
		t.Fatalf("got parent=%v err=%v, want not-found error", p, err)
	}
	_, wantMsg, _ := ResolveSession("no-such-session", f.all)
	if lpe := lpErr(t, err); lpe.Message != wantMsg || lpe.Code != ErrCodeNotFound {
		t.Fatalf("got %+v, want message %q code %q", lpe, wantMsg, ErrCodeNotFound)
	}
}

// With nest_under_parent on, a caller two levels down goes one hop up, to
// its own parent (itself a sub-session now that trees nest), never further.
func TestLaunchParent_NestOnFromADeepCallerGoesOneHop(t *testing.T) {
	f := newParentFixture()

	deep := session.NewInstanceWithGroupAndTool("Deep", "/proj/d", "g", "claude")
	deep.ID = "dep-0001"
	deep.SetParentWithPath(f.child.ID, f.child.ProjectPath)
	setCaller(t, deep.ID)
	p, by, note, err := selectLaunchParent("", false, nestOn, append(f.all, deep))
	if err != nil || p != f.child || by != deep || !strings.Contains(note, "linked under its parent Child") {
		t.Fatalf("two-level chain: got parent=%s by=%s note=%q err=%v, want the caller's parent Child", titleOf(p), titleOf(by), note, err)
	}
	p, _, _, err = selectLaunchParent(deep.Title, false, nestOn, append(f.all, deep))
	if err != nil || p != deep {
		t.Fatalf("explicit two-level chain: got parent=%s err=%v, want Deep itself", titleOf(p), err)
	}
}

func TestLaunchParent_DanglingParentStartsTopLevelWithNote(t *testing.T) {
	f := newParentFixture()
	orphan := session.NewInstanceWithGroupAndTool("Orphan", "/proj/o", "g", "claude")
	orphan.ID = "orp-0001"
	orphan.SetParentWithPath("gone-0001", "/proj/gone")
	setCaller(t, orphan.ID)
	p, by, note, err := selectLaunchParent("", false, nestOn, append(f.all, orphan))
	if p != nil || by != nil || err != nil {
		t.Fatalf("dangling parent: got parent=%v err=%v, want top-level and no error", p, err)
	}
	for _, want := range []string{orphan.ID, "gone-0001", "--no-parent"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note %q must mention %q", note, want)
		}
	}
}

func TestLaunchParent_PrecheckAcceptsDanglingParentCaller(t *testing.T) {
	_, _, profile := setupAddDefaultPathTest(t)
	f := seedParentRegistry(t, profile, "")
	orphan := session.NewInstanceWithGroupAndTool("Orphan", "/proj/o", "g", "claude")
	orphan.ID = "orp-0001"
	orphan.SetParentWithPath("gone-0001", "/proj/gone")
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	all := append(f.all, orphan)
	if err := storage.SaveWithGroups(all, session.NewGroupTreeWithGroups(all, nil)); err != nil {
		t.Fatal(err)
	}
	setCaller(t, orphan.ID)
	if err := validateStartupQueryCapacity(profile, "", "", t.TempDir(), false, true, true, nil); err != nil {
		t.Fatalf("dangling-parent caller refused by the pre-check: %v", err)
	}
}

// fakeTmux puts a tmux on PATH that records every call and answers with the
// tmux session name of the fixture's child. That name ends in a random suffix,
// not the session ID.
func fakeTmux(t *testing.T, child *session.Instance) (marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "probed")
	script := "#!/bin/sh\ntouch " + marker + "\necho " + child.GetTmuxSession().Name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "/tmp/fake,1,0")
	return marker
}

func TestLaunchParent_EnvIdentitySkipsTheTmuxProbe(t *testing.T) {
	f := newParentFixture()
	for _, noParent := range []bool{true, false} {
		marker := fakeTmux(t, f.child)
		t.Setenv("AGENT_DECK_SESSION_ID", "")
		t.Setenv("AGENTDECK_INSTANCE_ID", f.parent.ID)
		if _, _, note, err := selectLaunchParent("", noParent, nestOn, f.all); err != nil || note != "" {
			t.Fatalf("noParent=%v: note=%q err=%v, want neither", noParent, note, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("noParent=%v: tmux was probed although the environment named a top-level caller", noParent)
		}
	}
}

func TestLaunchParent_TmuxIdentityStillUsedWithoutEnv(t *testing.T) {
	f := newParentFixture()
	marker := fakeTmux(t, f.child)
	t.Setenv("AGENT_DECK_SESSION_ID", "")
	t.Setenv("AGENTDECK_INSTANCE_ID", "")
	p, _, note, err := selectLaunchParent("", true, nestOn, f.all)
	if err != nil || p != nil || !strings.Contains(note, f.parent.Title) {
		t.Fatalf("got parent=%v note=%q err=%v, want top-level with the note naming the parent", p, note, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("tmux was not probed with no environment identity: %v", err)
	}
}

func TestLaunchParent_NoParentFlagStaysTopLevel(t *testing.T) {
	f := newParentFixture()
	for _, tc := range []struct {
		name, caller string
		wantNote     bool
	}{
		{"no identity", "", false},
		{"top-level", f.parent.ID, false},
		{"sub-session", f.child.ID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCaller(t, tc.caller)
			p, _, note, err := selectLaunchParent("", true, nestOn, f.all)
			if err != nil || p != nil {
				t.Fatalf("got parent=%v err=%v, want top-level", p, err)
			}
			if tc.wantNote {
				if !strings.Contains(note, f.parent.Title) || !strings.Contains(note, "--no-parent") {
					t.Fatalf("note %q must name the parent %q and spell --no-parent", note, f.parent.Title)
				}
			} else if note != "" {
				t.Fatalf("unexpected note %q", note)
			}
		})
	}
}

func TestLaunchParent_NoIdentityStaysTopLevel(t *testing.T) {
	f := newParentFixture()
	setCaller(t, "")
	p, _, note, err := selectLaunchParent("", false, nestOn, f.all)
	if p != nil || note != "" || err != nil {
		t.Fatalf("got parent=%v note=%q err=%v, want none of them", p, note, err)
	}
}

func TestLaunchParent_StaleIdentityErrors(t *testing.T) {
	f := newParentFixture()
	setCaller(t, "stale-9999")
	p, _, _, err := selectLaunchParent("", false, nestOn, f.all)
	if p != nil || err == nil || !strings.Contains(err.Error(), "stale-9999") {
		t.Fatalf("got parent=%v err=%v, want an error naming the stale id", p, err)
	}
}

func TestLaunchParent_NoParentIgnoresStaleIdentity(t *testing.T) {
	f := newParentFixture()
	setCaller(t, "stale-9999")
	p, _, note, err := selectLaunchParent("", true, nestOn, f.all)
	if p != nil || note != "" || err != nil {
		t.Fatalf("got parent=%v note=%q err=%v, want top-level, no error, no note", p, note, err)
	}
}

func TestLaunchParent_SelectorErrorCarriesTheFailingID(t *testing.T) {
	f := newParentFixture()
	setCaller(t, "stale-9999")
	_, _, _, err := selectLaunchParent("", false, nestOn, f.all)
	if lpe := lpErr(t, err); lpe.UnresolvedID != "stale-9999" {
		t.Fatalf("UnresolvedID %q, want the stale id", lpe.UnresolvedID)
	}
}

func TestLaunchParent_ReplaceCloneCallShapesUnchanged(t *testing.T) {
	f := newParentFixture()
	// Clone of a sub-session: -parent=<its top-level parent>, launched from anywhere.
	for _, caller := range []string{"", f.parent.ID, f.child.ID} {
		setCaller(t, caller)
		p, _, note, err := selectLaunchParent(f.parent.Title, false, nestOn, f.all)
		if err != nil || p != f.parent || note != "" {
			t.Fatalf("sub-session clone from %q: parent=%v note=%q err=%v", caller, p, note, err)
		}
	}
	// Clone of a top-level session: -no-parent, launched from a top-level session or none.
	for _, caller := range []string{"", f.parent.ID, f.other.ID} {
		setCaller(t, caller)
		p, _, note, err := selectLaunchParent("", true, nestOn, f.all)
		if err != nil || p != nil || note != "" {
			t.Fatalf("top-level clone from %q: parent=%v note=%q err=%v", caller, p, note, err)
		}
	}
}

func TestLaunchParent_OnlyTheSelectorCallsTheAutoResolver(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range bytes.Split(raw, []byte("\n")) {
			s := strings.TrimSpace(string(line))
			if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "func resolveAutoParentInstanceChecked") {
				continue
			}
			if strings.Contains(s, "resolveAutoParentInstanceChecked(") {
				sites = append(sites, name)
				_ = i
			}
		}
	}
	if len(sites) != 1 || sites[0] != "launch_parent.go" {
		t.Fatalf("resolveAutoParentInstanceChecked call sites outside tests: %v, want exactly one, in launch_parent.go", sites)
	}
}

// seedParentRegistry stores the fixture (parent, child, other) in a profile.
func seedParentRegistry(t *testing.T, profile, parentPath string) parentFixture {
	t.Helper()
	return seedParentRegistryWith(t, profile, func(f *parentFixture) {
		if parentPath != "" {
			f.parent.ProjectPath = parentPath
		}
	})
}

// seedParentRegistryWith is seedParentRegistry with the fixture adjusted by
// edit before it is stored.
func seedParentRegistryWith(t *testing.T, profile string, edit func(*parentFixture)) parentFixture {
	t.Helper()
	f := newParentFixture()
	edit(&f)
	parentPath := f.parent.ProjectPath
	if parentPath == "/proj/parent" {
		parentPath = ""
	}
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatalf("NewStorageWithProfile: %v", err)
	}
	defer storage.Close()
	groupTree := session.NewGroupTreeWithGroups(f.all, []*session.GroupData{
		{Name: "parent-group", Path: "parent-group", Expanded: true, DefaultPath: parentPath},
		{Name: "child-group", Path: "child-group", Expanded: true},
		{Name: "other-group", Path: "other-group", Expanded: true},
	})
	if err := storage.SaveWithGroups(f.all, groupTree); err != nil {
		t.Fatalf("SaveWithGroups: %v", err)
	}
	return f
}

func TestLaunchParent_PrecheckAcceptsSubsessionCaller(t *testing.T) {
	home, _, profile := setupAddDefaultPathTest(t)
	writeAddUserConfig(t, home, nestUnderParentConfig)
	f := seedParentRegistry(t, profile, "")
	setCaller(t, f.child.ID)

	var group string
	if err := validateStartupQueryCapacity(profile, "", "", t.TempDir(), false, true, true, &group); err != nil {
		t.Fatalf("child caller refused by the pre-check: %v", err)
	}
	if group != "parent-group" {
		t.Fatalf("fallback group %q, want the parent's group, never the child's", group)
	}
	group = ""
	if err := validateStartupQueryCapacity(profile, "explicit-group", "", t.TempDir(), false, true, true, &group); err != nil {
		t.Fatalf("child caller with -g refused: %v", err)
	}
	if group != "explicit-group" {
		t.Fatalf("explicit group overridden: %q", group)
	}
	// The stale-id message is unchanged and still names the id.
	setCaller(t, "stale-9999")
	err := validateStartupQueryCapacity(profile, "", "", t.TempDir(), false, true, true, nil)
	if err == nil || err.Error() != `automatic parent "stale-9999" could not be resolved; use --no-parent` {
		t.Fatalf("stale-id pre-check message changed: %v", err)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	defer func() { os.Stderr = orig }()
	fn()
	_ = w.Close()
	return <-done
}

func TestLaunchParent_AddSubsessionCallerGroupFromParent(t *testing.T) {
	home, _, profile := setupAddDefaultPathTest(t)
	parentPath := filepath.Join(home, "parent-project")
	if err := os.MkdirAll(parentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	f := seedParentRegistry(t, profile, parentPath)
	writeAddUserConfig(t, home, nestUnderParentConfig)
	setCaller(t, f.child.ID)

	stderr := captureStderr(t, func() {
		captureStdout(t, func() { handleAdd(profile, []string{"--title", "helper", "--quiet"}) })
	})

	helper, hints := loadAddedHelper(t, profile)
	if hints[hintKeyParent] != f.parent.ID || hints[hintKeyLaunchedBy] != f.child.ID {
		t.Fatalf("hints %v, want parent=%s and launched-by=%s", hints, f.parent.ID, f.child.ID)
	}
	if helper.ParentSessionID != f.parent.ID {
		t.Fatalf("helper parent %q, want the parent %q", helper.ParentSessionID, f.parent.ID)
	}
	if helper.GroupPath != "parent-group" {
		t.Fatalf("helper group %q, want the parent's group, never the child's", helper.GroupPath)
	}
	if helper.ProjectPath != parentPath {
		t.Fatalf("helper path %q, want the parent group's default path %q", helper.ProjectPath, parentPath)
	}
	if !strings.Contains(stderr, "linked under its parent") {
		t.Fatalf("stderr %q lacks the note", stderr)
	}
}

func titleOf(inst *session.Instance) string {
	if inst == nil {
		return "<none>"
	}
	return inst.Title
}

const nestUnderParentConfig = "[launch]\nnest_under_parent = true\n"

// loadAddedHelper returns the stored session titled "helper" and its hints.
func loadAddedHelper(t *testing.T, profile string) (*session.Instance, map[string]string) {
	t.Helper()
	st, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insts, _, err := st.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range insts {
		if inst.Title == "helper" {
			hints, _, err := readInstanceHints(st.GetDB(), inst.ID)
			if err != nil {
				t.Fatal(err)
			}
			return inst, hints
		}
	}
	t.Fatal("helper not stored")
	return nil, nil
}

func TestLaunchParent_NestUnderParentConfigKey(t *testing.T) {
	home, _, _ := setupAddDefaultPathTest(t)
	if launchNestUnderParent() {
		t.Fatal("nest_under_parent is on without a config file; the default must be off")
	}
	writeAddUserConfig(t, home, "[launch]\ncontext_level = \"full\"\n")
	if launchNestUnderParent() {
		t.Fatal("nest_under_parent is on with a [launch] section that does not set it")
	}
	writeAddUserConfig(t, home, nestUnderParentConfig)
	if !launchNestUnderParent() {
		t.Fatal("[launch] nest_under_parent = true is not honoured")
	}
}

func TestLaunchParent_MaxParentDepthConfigKey(t *testing.T) {
	home, _, _ := setupAddDefaultPathTest(t)
	if got := launchMaxParentDepth(); got != session.DefaultMaxParentDepth {
		t.Fatalf("cap without a config file = %d, want the default %d", got, session.DefaultMaxParentDepth)
	}
	for _, tc := range []struct {
		value string
		want  int
	}{{"1", 1}, {"5", 5}, {"0", session.DefaultMaxParentDepth}, {"-2", session.DefaultMaxParentDepth}} {
		writeAddUserConfig(t, home, "[launch]\nmax_parent_depth = "+tc.value+"\n")
		if got := launchMaxParentDepth(); got != tc.want {
			t.Fatalf("max_parent_depth = %s gives cap %d, want %d", tc.value, got, tc.want)
		}
	}
}

// With nest_under_parent off (the default) the calling session is the parent,
// top-level or not, with no note and no launched-by; --no-parent is silent; a
// stale caller id still errors.
func TestLaunchParent_DefaultNestsUnderTheCaller(t *testing.T) {
	f := newParentFixture()
	orphan := session.NewInstanceWithGroupAndTool("Orphan", "/proj/o", "g", "claude")
	orphan.ID = "orp-0001"
	orphan.SetParentWithPath("gone-0001", "/proj/gone")
	deep := session.NewInstanceWithGroupAndTool("Deep", "/proj/d", "g", "claude")
	deep.ID = "dep-0001"
	deep.SetParentWithPath(f.child.ID, f.child.ProjectPath)
	all := append(f.all, orphan, deep)
	for _, tc := range []struct {
		name, caller string
		noParent     bool
		want         *session.Instance
	}{
		{"sub-session caller", f.child.ID, false, f.child},
		{"sub-session caller --no-parent", f.child.ID, true, nil},
		{"dangling-parent caller", orphan.ID, false, orphan},
		{"two-level caller", deep.ID, false, deep},
		{"top-level caller", f.parent.ID, false, f.parent},
		{"top-level caller --no-parent", f.parent.ID, true, nil},
		{"no identity", "", false, nil},
		{"stale id --no-parent", "stale-9999", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCaller(t, tc.caller)
			p, by, note, err := selectLaunchParent("", tc.noParent, nestOff, all)
			if err != nil || p != tc.want || by != nil || note != "" {
				t.Fatalf("got parent=%s launched-by=%s note=%q err=%v, want parent=%s and nothing else", titleOf(p), titleOf(by), note, err, titleOf(tc.want))
			}
		})
	}
	setCaller(t, "stale-9999")
	if _, _, _, err := selectLaunchParent("", false, nestOff, all); err == nil || lpErr(t, err).UnresolvedID != "stale-9999" {
		t.Fatalf("stale caller id: err=%v, want the unresolved-id error", err)
	}
}

func TestLaunchParent_DefaultOffNoParentSkipsTheTmuxProbe(t *testing.T) {
	f := newParentFixture()
	marker := fakeTmux(t, f.child)
	t.Setenv("AGENT_DECK_SESSION_ID", "")
	t.Setenv("AGENTDECK_INSTANCE_ID", "")
	if p, _, note, err := selectLaunchParent("", true, nestOff, f.all); p != nil || note != "" || err != nil {
		t.Fatalf("got parent=%v note=%q err=%v, want top-level and nothing else", p, note, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("--no-parent probed tmux with nest_under_parent off")
	}
}

// The record `add` writes from inside a sub-session with no config: the
// calling sub-session is the parent (a grandchild of its parent), the group
// is the caller's as for any child under `add`, the parent hint names the
// caller, and there is no launched-by hint and no note.
func TestLaunchParent_AddDefaultSubsessionCallerBecomesTheParent(t *testing.T) {
	home, _, profile := setupAddDefaultPathTest(t)
	parentPath := filepath.Join(home, "parent-project")
	if err := os.MkdirAll(parentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(home, "child-project")
	if err := os.MkdirAll(childPath, 0o755); err != nil {
		t.Fatal(err)
	}
	f := seedParentRegistryWith(t, profile, func(f *parentFixture) {
		f.parent.ProjectPath = parentPath
		f.child.ProjectPath = childPath
	})
	setCaller(t, f.child.ID)

	stderr := captureStderr(t, func() {
		captureStdout(t, func() { handleAdd(profile, []string{"--title", "helper", "--quiet"}) })
	})

	helper, hints := loadAddedHelper(t, profile)
	if helper.ParentSessionID != f.child.ID {
		t.Fatalf("helper parent %q, want the calling sub-session %q", helper.ParentSessionID, f.child.ID)
	}
	if helper.GroupPath != "child-group" {
		t.Fatalf("helper group %q, want the caller's group", helper.GroupPath)
	}
	if hints[hintKeyParent] != f.child.ID {
		t.Fatalf("hints %v, want parent=%s", hints, f.child.ID)
	}
	if _, ok := hints[hintKeyLaunchedBy]; ok {
		t.Fatalf("hints %v carry launched-by", hints)
	}
	if strings.Contains(stderr, "linked under") {
		t.Fatalf("stderr %q carries a parent note", stderr)
	}
}

func TestLaunchParent_PrecheckDefaultSubsessionCallerUsesItsGroup(t *testing.T) {
	_, _, profile := setupAddDefaultPathTest(t)
	f := seedParentRegistry(t, profile, "")
	setCaller(t, f.child.ID)
	path := t.TempDir()
	var group string
	if err := validateStartupQueryCapacity(profile, "", "", path, false, true, true, &group); err != nil {
		t.Fatalf("child caller refused by the pre-check: %v", err)
	}
	if group != "child-group" {
		t.Fatalf("pre-check group %q, want the calling sub-session's group", group)
	}
	// Without --inherit-group the folder-derived group stands, as for any
	// parent.
	group = ""
	if err := validateStartupQueryCapacity(profile, "", "", path, false, false, true, &group); err != nil {
		t.Fatalf("child caller refused by the pre-check: %v", err)
	}
	if want := session.GroupPathForProject(path); group != want {
		t.Fatalf("pre-check group %q, want the folder-derived %q", group, want)
	}
}
