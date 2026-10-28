package ui

// One row per Epic: a folded parent row rolls up what its subtree needs from
// the person, rows stay quiet (no repeated pane title, no default account),
// the conductor mark (◆) comes from metadata, never from the title, and a
// driver-set row state (session set <id> state) gives the row one symbol.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// makeEpicTree builds conductor → epic → {build, review → probe} in one group.
func makeEpicTree() (*Home, map[string]*session.Instance) {
	h := seamBNewHome()
	h.parentChildrenCollapsed = make(map[string]bool)
	mk := func(id, parent string) *session.Instance {
		inst := session.NewInstanceWithTool(id, "/tmp/"+id, "claude")
		inst.ID = id
		inst.GroupPath = "crew"
		inst.Status = session.StatusIdle
		if parent != "" {
			inst.SetParent(parent)
		}
		return inst
	}
	all := map[string]*session.Instance{
		"lead":   mk("lead", ""),
		"epic":   mk("epic", "lead"),
		"build":  mk("build", "epic"),
		"review": mk("review", "epic"),
		"probe":  mk("probe", "review"),
	}
	all["lead"].IsConductor = true
	h.groupTree = session.NewGroupTree([]*session.Instance{all["lead"], all["epic"], all["build"], all["review"], all["probe"]})
	return h, all
}

// epicRow renders the row of id from the current flat list with snap.
func epicRow(t *testing.T, h *Home, id string, snap map[string]sessionRenderState, width int) string {
	t.Helper()
	for _, item := range h.flatItems {
		if item.Type == session.ItemTypeSession && item.Session != nil && item.Session.ID == id {
			var b strings.Builder
			h.renderSessionItem(&b, item, false, snap, width)
			return ansi.Strip(b.String())
		}
	}
	t.Fatalf("row %q not in the flat list", id)
	return ""
}

func idleSnap(all map[string]*session.Instance) map[string]sessionRenderState {
	snap := make(map[string]sessionRenderState, len(all))
	for id, inst := range all {
		snap[id] = sessionRenderState{status: session.StatusIdle, tool: "claude", title: inst.Title}
	}
	return snap
}

func TestFoldedRowRollsUpNeedsYou(t *testing.T) {
	needs := map[string]func(*sessionRenderState){
		"ask": func(s *sessionRenderState) { s.askKind = "pick" },
		"open menu": func(s *sessionRenderState) {
			s.status, s.substate = session.StatusWaiting, session.SubstateInteractiveMenu
		},
		"error": func(s *sessionRenderState) { s.status = session.StatusError },
		"driver ▲": func(s *sessionRenderState) {
			s.rowState = &session.RowState{Symbol: "▲", Attention: session.AttentionNeedsYou, Word: "your approval"}
		},
	}
	for name, mark := range needs {
		t.Run(name, func(t *testing.T) {
			h, all := makeEpicTree()
			snap := idleSnap(all)
			probe := snap["probe"] // two levels below the epic row
			mark(&probe)
			snap["probe"] = probe
			build := snap["build"]
			build.status = session.StatusRunning
			snap["build"] = build

			h.parentChildrenCollapsed["epic"] = true
			h.rebuildFlatItems()
			got := epicRow(t, h, "epic", snap, 160)
			if !strings.Contains(got, "▸2 ! 1 needs you · 1 working") {
				t.Fatalf("folded epic row lacks the roll-up, got %q", got)
			}
		})
	}
}

func TestRollupOnlyOnFoldedRows(t *testing.T) {
	h, all := makeEpicTree()
	snap := idleSnap(all)
	probe := snap["probe"]
	probe.askKind = "approval"
	snap["probe"] = probe

	h.rebuildFlatItems()
	if got := epicRow(t, h, "epic", snap, 160); strings.Contains(got, "needs you") || strings.Contains(got, " !") {
		t.Fatalf("an unfolded row must not roll up (its children show), got %q", got)
	}

	h.parentChildrenCollapsed["epic"] = true
	h.rebuildFlatItems()
	quiet := idleSnap(all)
	if got := epicRow(t, h, "epic", quiet, 160); strings.Contains(got, "needs you") || strings.Contains(got, "working") || strings.Contains(got, " !") {
		t.Fatalf("a folded row over an idle subtree must stay quiet, got %q", got)
	}
}

// The mark stays when the row is too narrow for the words.
func TestRollupMarkSurvivesNarrowRow(t *testing.T) {
	h, all := makeEpicTree()
	snap := idleSnap(all)
	probe := snap["probe"]
	probe.status = session.StatusError
	snap["probe"] = probe
	h.parentChildrenCollapsed["epic"] = true
	h.rebuildFlatItems()
	got := epicRow(t, h, "epic", snap, 34)
	if !strings.Contains(got, "▸2 !") {
		t.Fatalf("narrow folded row lost the needs-you mark, got %q", got)
	}
	if strings.Contains(got, "needs you") {
		t.Fatalf("narrow row should drop the words before the title, got %q", got)
	}
}

// Rows keep their order whatever their subtree needs.
func TestRollupKeepsRowOrder(t *testing.T) {
	h, all := makeEpicTree()
	h.rebuildFlatItems()
	before := strings.Join(flatSessionTitles(h), ",")
	all["probe"].Status = session.StatusError
	all["build"].Status = session.StatusRunning
	h.rebuildFlatItems()
	if after := strings.Join(flatSessionTitles(h), ","); after != before {
		t.Fatalf("row order moved: %s -> %s", before, after)
	}
}

func TestConductorMarkFromMetadata(t *testing.T) {
	h, all := makeEpicTree()
	// A worker whose title claims the role carries no flag and no children.
	all["build"].Title = "conductor-crew-9 ◆ conducting"
	h.refreshSessionRenderSnapshot([]*session.Instance{all["lead"], all["epic"], all["build"], all["review"], all["probe"]})
	snap := h.getSessionRenderSnapshot()
	want := map[string]bool{"lead": true, "epic": true, "build": false, "review": false, "probe": false}
	for id, mark := range want {
		if snap[id].conductorMark != mark {
			t.Errorf("%s: conductorMark = %v, want %v", id, snap[id].conductorMark, mark)
		}
	}

	h.rebuildFlatItems()
	if got := epicRow(t, h, "epic", snap, 160); !strings.Contains(got, "◆ epic") {
		t.Errorf("epic conductor row lacks ◆, got %q", got)
	}
	if got := epicRow(t, h, "review", snap, 160); strings.Contains(got, "◆") {
		t.Errorf("a worker with a child of its own is not a conductor, got %q", got)
	}
}

func TestSubtitleRepeatsTitle(t *testing.T) {
	tests := []struct {
		subtitle, title string
		want            bool
	}{
		{"conductor-crew-7", "conductor-crew-7 ◆ conducting · 3/7 landed", true},
		{"X-767 layer 4", "X-767 working · build · layer 4", true},
		{"✓ X-764 merged build worker", "X-764 merged · build · worker", true},
		{"Refactor the parser", "X-764 merged · build · worker", false},
		{"", "anything", false},
	}
	for _, tt := range tests {
		if got := subtitleRepeatsTitle(tt.subtitle, tt.title); got != tt.want {
			t.Errorf("subtitleRepeatsTitle(%q, %q) = %v, want %v", tt.subtitle, tt.title, got, tt.want)
		}
	}
}

// The row stops repeating its own title after the badges.
func TestRowDropsRepeatedPaneTitle(t *testing.T) {
	h := NewHome()
	h.width, h.height = 240, 40
	h.showPaneTitles = true
	inst := &session.Instance{ID: "w", Title: "X-767 working · build · layer 4", Tool: "claude", Status: session.StatusRunning}
	row := func(pane string) string {
		var b strings.Builder
		snap := map[string]sessionRenderState{inst.ID: {status: inst.Status, tool: "claude", title: inst.Title, paneTitle: pane}}
		h.renderSessionItem(&b, session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, Path: "crew", IsLastInGroup: true}, false, snap, 240)
		return ansi.Strip(b.String())
	}
	if got := row("X-767 layer 4"); strings.Count(got, "layer 4") != 1 {
		t.Errorf("repeated pane title still trails the row: %q", got)
	}
	if got := row("Refactor the parser"); !strings.Contains(got, "Refactor the parser") {
		t.Errorf("a pane title that says something new must stay: %q", got)
	}
}

func TestHideDefaultAccountBadge(t *testing.T) {
	writeBadgeConfig(t, "[display]\nhide_default_account_badge = true\n\n"+
		"[profiles.team.claude]\nconfig_dir = \"/tmp/claude-team\"\n\n"+
		"[profiles.side.claude]\nconfig_dir = \"/tmp/claude-side\"\n\n"+
		"[groups.\"crew\".claude]\nconfig_dir = \"/tmp/claude-team\"\n")
	h := newBadgeHome(t)
	onGroup := &session.Instance{ID: "on", Title: "on-group", Tool: "claude", GroupPath: "crew", Account: "team", Status: session.StatusIdle}
	offGroup := &session.Instance{ID: "off", Title: "off-group", Tool: "claude", GroupPath: "crew", Account: "side", Status: session.StatusIdle}
	h.refreshSessionRenderSnapshot([]*session.Instance{onGroup, offGroup})
	snap := h.getSessionRenderSnapshot()
	row := func(inst *session.Instance) string {
		var b strings.Builder
		h.renderSessionItem(&b, session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, Path: "crew", IsLastInGroup: true}, false, snap, 240)
		return ansi.Strip(b.String())
	}
	if got := row(onGroup); strings.Contains(got, "[account:") {
		t.Errorf("the group's own account must be hidden, row = %q", got)
	}
	if got := row(offGroup); !strings.Contains(got, `[account:"side"]`) {
		t.Errorf("an account the group would not pick must show, row = %q", got)
	}
}

func TestDefaultAccountBadgeShownWhenOptionOff(t *testing.T) {
	writeBadgeConfig(t, "[profiles.team.claude]\nconfig_dir = \"/tmp/claude-team\"\n\n"+
		"[groups.\"crew\".claude]\nconfig_dir = \"/tmp/claude-team\"\n")
	h := newBadgeHome(t)
	inst := &session.Instance{ID: "on", Title: "on-group", Tool: "claude", GroupPath: "crew", Account: "team", Status: session.StatusIdle}
	h.refreshSessionRenderSnapshot([]*session.Instance{inst})
	var b strings.Builder
	h.renderSessionItem(&b, session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, Path: "crew", IsLastInGroup: true}, false, h.getSessionRenderSnapshot(), 240)
	if got := ansi.Strip(b.String()); !strings.Contains(got, `[account:"team"]`) {
		t.Errorf("option off: the account badge must render, row = %q", got)
	}
}

// A driver state's symbol replaces the status icon (one symbol per row), its
// word and role trail the title; unset, the row is the deck's own.
func TestRowStateReplacesStatusIcon(t *testing.T) {
	h := NewHome()
	h.width, h.height = 240, 40
	inst := &session.Instance{ID: "w", Title: "X-12 parser", Tool: "claude", Status: session.StatusWaiting}
	row := func(st *session.RowState) string {
		var b strings.Builder
		snap := map[string]sessionRenderState{inst.ID: {status: inst.Status, tool: "claude", title: inst.Title, rowState: st}}
		h.renderSessionItem(&b, session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, Path: "crew", IsLastInGroup: true}, false, snap, 240)
		return ansi.Strip(b.String())
	}
	if plain := row(nil); !strings.Contains(plain, "◐ X-12 parser") {
		t.Fatalf("unset state must keep the deck's glyph, got %q", plain)
	}
	got := row(&session.RowState{Symbol: "▲", Attention: session.AttentionNeedsYou, Word: "your approval", Role: "review"})
	if !strings.Contains(got, "▲ X-12 parser your approval · review") {
		t.Fatalf("driver state row = %q", got)
	}
	if strings.Contains(got, "◐") {
		t.Fatalf("two symbols on one row: %q", got)
	}
	if bare := row(&session.RowState{Symbol: "⇡", Attention: session.AttentionMoving}); !strings.Contains(bare, "⇡ X-12 parser") || strings.Contains(bare, "·") {
		t.Fatalf("symbol-only state = %q", bare)
	}
}

func TestRowStateGlyphColourFollowsAttention(t *testing.T) {
	fg := func(c interface{}) string { return fmt.Sprintf("%v", c) }
	for attention, want := range map[string]string{
		session.AttentionNeedsYou: fg(ColorYellow),
		session.AttentionMoving:   fg(SessionStatusRunning.GetForeground()),
		session.AttentionQuiet:    fg(SessionStatusIdle.GetForeground()),
	} {
		_, style, ok := rowStateGlyph(&session.RowState{Symbol: "x", Attention: attention}, false)
		if !ok || fg(style.GetForeground()) != want {
			t.Errorf("%s: colour %v, want %v", attention, style.GetForeground(), want)
		}
	}
	if _, _, ok := rowStateGlyph(&session.RowState{Symbol: "▲", Attention: session.AttentionNeedsYou}, true); ok {
		t.Error("an archived row must keep the stopped glyph")
	}
	if _, _, ok := rowStateGlyph(nil, false); ok {
		t.Error("no state must keep the deck's glyph")
	}
}

// A driver's moving/quiet state decides "working" in the roll-up; the deck's
// own needs-you signals still count under a quiet driver state.
func TestRollupFollowsDriverAttention(t *testing.T) {
	h, all := makeEpicTree()
	snap := idleSnap(all)
	build := snap["build"]
	build.status = session.StatusRunning // the deck says running, the driver quiet: not working
	build.rowState = &session.RowState{Symbol: "○", Attention: session.AttentionQuiet}
	snap["build"] = build
	review := snap["review"]
	review.rowState = &session.RowState{Symbol: "⇡", Attention: session.AttentionMoving, Word: "landing"}
	snap["review"] = review
	probe := snap["probe"]
	probe.status = session.StatusError
	probe.rowState = &session.RowState{Symbol: "■", Attention: session.AttentionQuiet}
	snap["probe"] = probe
	h.parentChildrenCollapsed["epic"] = true
	h.rebuildFlatItems()
	if got := epicRow(t, h, "epic", snap, 160); !strings.Contains(got, "▸2 ! 1 needs you · 1 working") {
		t.Fatalf("roll-up = %q", got)
	}
}

// The refresher carries the stored row state into the render snapshot.
func TestRefreshSnapshotCarriesRowState(t *testing.T) {
	h := NewHome()
	inst := &session.Instance{ID: "s", Title: "x", Tool: "claude"}
	inst.SetRowState(&session.RowState{Symbol: "▲", Attention: session.AttentionNeedsYou})
	h.refreshSessionRenderSnapshot([]*session.Instance{inst, {ID: "plain", Title: "y", Tool: "claude"}})
	snap := h.getSessionRenderSnapshot()
	if st := snap["s"].rowState; st == nil || st.Symbol != "▲" || snap["plain"].rowState != nil {
		t.Fatalf("snapshot rowState: s=%+v plain=%+v", st, snap["plain"].rowState)
	}
}

func TestGroupItemTitle(t *testing.T) {
	tests := []struct {
		title, ref, want string
	}{
		{"PROJ-507 one-fresh-picture", "PROJ-507", "507 one fresh picture"},
		{"proj-507: one-fresh-picture", "PROJ-507", "507 one fresh picture"},
		{"PROJ-507", "PROJ-507", "507"},
		{"PROJ-5070 other", "PROJ-507", "507 PROJ-5070 other"}, // not the same ref: title kept whole
		{"Payments rework", "PROJ-507", "507 Payments rework"},
	}
	for _, tt := range tests {
		_, number, ok := itemRefPrefixNumber(tt.ref)
		if !ok {
			t.Fatalf("itemRefPrefixNumber(%q) not ok", tt.ref)
		}
		if got := groupItemTitle(tt.title, tt.ref, number); got != tt.want {
			t.Errorf("groupItemTitle(%q, %q) = %q, want %q", tt.title, tt.ref, got, tt.want)
		}
	}
	for _, ref := range []string{"https://tracker.example/items/42", "PROJ-", "PROJ-12a", "507"} {
		if _, _, ok := itemRefPrefixNumber(ref); ok {
			t.Errorf("itemRefPrefixNumber(%q) ok; want no number shape", ref)
		}
	}
	if prefix, number, _ := itemRefPrefixNumber("my-team-42"); prefix != "my-team-" || number != "42" {
		t.Errorf("prefix/number = %q/%q", prefix, number)
	}
}

// Inside a group whose items share one prefix the row shows the number; a
// mixed group, a row with no item and the stored title stay as they are.
func TestGroupSharedItemPrefixShowsNumber(t *testing.T) {
	mk := func(id, group, title, item string) *session.Instance {
		inst := &session.Instance{ID: id, Title: title, Tool: "claude", GroupPath: group, Status: session.StatusIdle}
		inst.SetItemRef(item)
		return inst
	}
	one := mk("a", "proj", "PROJ-507 one-fresh-picture", "PROJ-507")
	two := mk("b", "proj", "PROJ-512 widgets", "PROJ-512")
	none := mk("c", "proj", "research notes", "")
	mixedA := mk("d", "mixed", "PROJ-9 thing", "PROJ-9")
	mixedB := mk("e", "mixed", "OTHER-3 thing", "OTHER-3")
	all := []*session.Instance{one, two, none, mixedA, mixedB}
	h := NewHome()
	h.width, h.height = 240, 40
	h.refreshSessionRenderSnapshot(all)
	snap := h.getSessionRenderSnapshot()
	row := func(inst *session.Instance) string {
		var b strings.Builder
		h.renderSessionItem(&b, session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, Path: inst.GroupPath, IsLastInGroup: true}, false, snap, 240)
		return ansi.Strip(b.String())
	}
	if got := row(one); !strings.Contains(got, " 507 one fresh picture") || strings.Contains(got, "PROJ-507") {
		t.Errorf("shared-prefix row = %q", got)
	}
	if got := row(two); !strings.Contains(got, " 512 widgets") {
		t.Errorf("shared-prefix row = %q", got)
	}
	if got := row(none); !strings.Contains(got, "research notes") {
		t.Errorf("row with no item = %q", got)
	}
	if got := row(mixedA); !strings.Contains(got, "PROJ-9 thing") {
		t.Errorf("mixed-prefix group must keep full titles, row = %q", got)
	}
	if one.Title != "PROJ-507 one-fresh-picture" {
		t.Errorf("stored title changed: %q", one.Title)
	}
}
