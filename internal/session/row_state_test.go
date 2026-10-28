package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// The documented set: one meaning per symbol.
var documentedRowStates = []RowState{
	{Symbol: "?", Attention: AttentionNeedsYou, Word: "asks you"},
	{Symbol: "▲", Attention: AttentionNeedsYou, Word: "your approval"},
	{Symbol: "✖", Attention: AttentionNeedsYou, Word: "error"},
	{Symbol: "◐", Attention: AttentionNeedsYou, Word: "needs your read"},
	{Symbol: "●", Attention: AttentionMoving, Word: "working"},
	{Symbol: "⇡", Attention: AttentionMoving, Word: "landing"},
	{Symbol: "○", Attention: AttentionQuiet, Word: "idle"},
	{Symbol: "■", Attention: AttentionQuiet, Word: "stopped"},
}

func TestParseRowState(t *testing.T) {
	for _, want := range documentedRowStates {
		raw, _ := json.Marshal(want)
		got, err := ParseRowState(string(raw))
		if err != nil || got == nil || *got != want {
			t.Errorf("ParseRowState(%s) = %+v, %v", raw, got, err)
		}
	}
	got, err := ParseRowState(` {"symbol":" ▲ ","attention":"needs-you","word":" your approval ","role":"review"} `)
	if err != nil || *got != (RowState{Symbol: "▲", Attention: AttentionNeedsYou, Word: "your approval", Role: "review"}) {
		t.Fatalf("trimmed parse = %+v, %v", got, err)
	}
	for _, clear := range []string{"", "  ", "null", "{}"} {
		if st, err := ParseRowState(clear); st != nil || err != nil {
			t.Errorf("ParseRowState(%q) = %+v, %v; want a clear", clear, st, err)
		}
	}
	bad := []string{
		`{"symbol":"▲"}`,                                    // no attention
		`{"symbol":"▲","attention":"urgent"}`,               // unknown attention
		`{"attention":"quiet"}`,                             // no symbol
		`{"symbol":"▲▲","attention":"quiet"}`,               // two symbols
		`{"symbol":"▲","attention":"quiet","colour":"red"}`, // unknown key
		`{"symbol":"▲","attention":"quiet","word":"a\nb"}`,  // control character
		`{"symbol":"▲","attention":"quiet","word":"` + strings.Repeat("w", MaxRowStateWordCells+1) + `"}`,
		`{"symbol":"▲","attention":"quiet","role":"` + strings.Repeat("r", MaxRowStateRoleCells+1) + `"}`,
		`▲ needs-you`, // not JSON
		`{"symbol":"▲","attention":"quiet"} {"symbol":"●","attention":"moving"}`,
	}
	for _, value := range bad {
		if st, err := ParseRowState(value); err == nil {
			t.Errorf("ParseRowState(%q) accepted: %+v", value, st)
		}
	}
}

func TestRowStateToolDataRoundTrip(t *testing.T) {
	st := &RowState{Symbol: "▲", Attention: AttentionNeedsYou, Word: "your approval"}
	td := json.RawMessage(`{"claude_session_id":"abc","item_ref":"X-1"}`)
	on := WriteRowStateToToolData(td, st, false)
	if got := ReadRowStateFromToolData(on); got == nil || *got != *st {
		t.Fatalf("state = %+v: %s", got, on)
	}
	if ReadItemRefFromToolData(on) != "X-1" {
		t.Fatalf("unrelated key lost: %s", on)
	}
	off := WriteRowStateToToolData(on, nil, true)
	if ReadRowStateFromToolData(off) != nil || !strings.Contains(string(off), `"row_state":null`) {
		t.Fatalf("clear must write an explicit null: %s", off)
	}
	merged := statedb.MergeToolDataExtras(on, WriteRowStateToToolData(json.RawMessage(`{}`), nil, true))
	if got := ReadRowStateFromToolData(merged); got != nil {
		t.Fatalf("clear reverted by the extras merge: %+v in %s", got, merged)
	}
	kept := statedb.MergeToolDataExtras(on, WriteRowStateToToolData(json.RawMessage(`{}`), nil, false))
	if got := ReadRowStateFromToolData(kept); got == nil || *got != *st {
		t.Fatalf("stored state dropped by a save that never touched it: %+v", got)
	}
	if got := WriteRowStateToToolData(json.RawMessage(`{"a":1}`), nil, false); strings.Contains(string(got), "row_state") {
		t.Fatalf("row without a state gained a key: %s", got)
	}
	if ReadRowStateFromToolData(nil) != nil || ReadRowStateFromToolData(json.RawMessage(`{"row_state":{"symbol":"x","attention":"loud"}}`)) != nil {
		t.Fatal("legacy or invalid stored state read as set")
	}
}

func TestSetFieldState(t *testing.T) {
	inst := NewInstance("worker", t.TempDir())
	if _, _, err := SetField(inst, FieldState, `{"symbol":"⇡","attention":"moving","word":"landing"}`, nil); err != nil {
		t.Fatalf("SetField state: %v", err)
	}
	if got := inst.RowState(); got == nil || got.Symbol != "⇡" || got.Attention != AttentionMoving || got.Word != "landing" {
		t.Fatalf("RowState = %+v", got)
	}
	old, _, err := SetField(inst, FieldState, "", nil)
	if err != nil || !strings.Contains(old, `"symbol":"⇡"`) || inst.RowState() != nil || !inst.rowStateCleared {
		t.Fatalf("clear: old=%q err=%v state=%+v cleared=%v", old, err, inst.RowState(), inst.rowStateCleared)
	}
	if _, _, err := SetField(inst, FieldState, `{"symbol":"!","attention":"loud"}`, nil); err == nil {
		t.Error("invalid state accepted")
	}
	if RestartPolicyFor(FieldState) != FieldLive {
		t.Error("state must be a live field")
	}
}

// The row state survives a save and a reload through the real storage.
func TestRowStatePersistsThroughStorage(t *testing.T) {
	setupSessionXDGPathEnv(t)
	storage, err := NewStorageWithProfile("rowstate")
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	inst := NewInstance("worker", t.TempDir())
	inst.SetRowState(&RowState{Symbol: "▲", Attention: AttentionNeedsYou, Word: "your approval", Role: "review"})
	if err := storage.Save([]*Instance{inst}); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := storage.Load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load: %v (%d rows)", err, len(loaded))
	}
	if got := loaded[0].RowState(); got == nil || got.Symbol != "▲" || got.Role != "review" {
		t.Fatalf("reloaded RowState = %+v", got)
	}
	loaded[0].SetRowState(nil)
	if err := storage.Save(loaded); err != nil {
		t.Fatalf("save clear: %v", err)
	}
	again, err := storage.Load()
	if err != nil || len(again) != 1 || again[0].RowState() != nil {
		t.Fatalf("clear did not persist: %v %+v", err, again)
	}
}
