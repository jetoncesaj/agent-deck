package ui

import (
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// The preview header and the info card show the session's tracker item
// when one is set, and nothing extra when it is not.
func TestPreviewShowsItemRef(t *testing.T) {
	inst := session.NewInstance("worker", t.TempDir())
	if got := itemRefLine(inst, 40); got != "" {
		t.Fatalf("no item: line = %q, want empty", got)
	}
	inst.SetItemRef("ONAZA-737")
	if got := itemRefLine(inst, 40); !strings.Contains(got, "ONAZA-737") {
		t.Fatalf("item line = %q", got)
	}
	inst.SetItemRef(strings.Repeat("x", 200))
	if got := itemRefLine(inst, 20); cellWidth(got) > 20 {
		t.Fatalf("item line %q is %d cells wide, want <= 20", got, cellWidth(got))
	}

	h := &Home{}
	inst.SetItemRef("ROONA-12")
	if card := h.renderSessionInfoCard(inst, 60, 20); !strings.Contains(card, "ROONA-12") {
		t.Fatalf("info card lacks the item:\n%s", card)
	}
}
