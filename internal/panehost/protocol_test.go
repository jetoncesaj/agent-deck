package panehost

import (
	"reflect"
	"testing"
)

func TestInputParserFeed(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{"left click 1-based to 0-based", []string{"\x1b[<0;5;3M"}, []string{"click 4 2"}},
		{"release is not an event", []string{"\x1b[<0;5;3m"}, nil},
		{"right and middle", []string{"\x1b[<2;1;1M\x1b[<1;10;20M"}, []string{"rclick 0 0", "mclick 9 19"}},
		{"wheel", []string{"\x1b[<64;7;8M\x1b[<65;7;9M"}, []string{"scroll up 6 7", "scroll down 6 8"}},
		{"modifiers dropped", []string{"\x1b[<16;2;2M"}, []string{"click 1 1"}},
		{"motion ignored", []string{"\x1b[<32;2;2M"}, nil},
		{"wide coordinates", []string{"\x1b[<0;300;90M"}, []string{"click 299 89"}},
		{"split across reads", []string{"\x1b[<0;1", "2;4M"}, []string{"click 11 3"}},
		{"split after ESC [", []string{"\x1b[", "A"}, []string{"key up"}},
		{"keys", []string{"q\r\t\x7f "}, []string{"key q", "key enter", "key tab", "key backspace", "key space"}},
		{"ctrl key", []string{"\x03"}, []string{"key C-c"}},
		{"arrows csi and ss3", []string{"\x1b[B\x1bOC"}, []string{"key down", "key right"}},
		{"page keys", []string{"\x1b[5~\x1b[6~\x1b[3~"}, []string{"key pgup", "key pgdn", "key delete"}},
		{"lone escape", []string{"\x1b"}, []string{"key escape"}},
		{"alt key", []string{"\x1bx"}, []string{"key escape", "key x"}},
		{"utf8 dropped", []string{"é"}, nil},
		{"unknown csi dropped", []string{"\x1b[?25h"}, nil},
		{"mouse then key", []string{"\x1b[<0;1;1Mj"}, []string{"click 0 0", "key j"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p InputParser
			var got []string
			for _, c := range tt.chunks {
				got = append(got, p.Feed([]byte(c))...)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Feed(%q) = %q, want %q", tt.chunks, got, tt.want)
			}
		})
	}
}

func TestDecideLayout(t *testing.T) {
	l := LayoutSpec{Width: "36%", NarrowCols: 100, MinCols: 30}
	tests := []struct {
		name               string
		width              int
		zoomed, hostZoomed bool
		want               LayoutDecision
	}{
		{"wide, nothing to do", 200, false, false, LayoutNone},
		{"phone width zooms", 60, false, false, LayoutZoom},
		{"already zoomed stays", 60, true, true, LayoutNone},
		{"zoomed by the person while narrow is left", 60, true, false, LayoutNone},
		{"wide again restores own zoom", 200, true, true, LayoutRestore},
		{"wide with the person's zoom is left", 200, true, false, LayoutNone},
		// 36% of 82 is 29 columns: too thin, hide it even above narrow_cols.
		{"column below min cols", 82, false, false, LayoutZoom},
		{"exactly at threshold is wide", 100, false, false, LayoutNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ll := l
			if tt.name == "column below min cols" {
				ll.NarrowCols = 80
			}
			if got := decideLayout(tt.width, tt.zoomed, tt.hostZoomed, ll); got != tt.want {
				t.Fatalf("decideLayout(%d, zoomed=%v, host=%v) = %v, want %v", tt.width, tt.zoomed, tt.hostZoomed, got, tt.want)
			}
		})
	}
}

func TestWidthPercent(t *testing.T) {
	for in, want := range map[string]int{"36%": 36, "40": 40, " 25% ": 25, "": 36, "abc": 36, "2%": 36, "95%": 36} {
		if got := widthPercent(in); got != want {
			t.Errorf("widthPercent(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestChangedKeys(t *testing.T) {
	a := map[string]string{"status": "running", "title": "w1", "item": ""}
	b := map[string]string{"status": "waiting", "title": "w1", "item": "", "pr": "12"}
	if got, want := changedKeys(a, b), []string{"pr", "status"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changedKeys = %v, want %v", got, want)
	}
	if got := changedKeys(a, nil); got != nil {
		t.Fatalf("a failed read must report nothing, got %v", got)
	}
	if got := changedKeys(nil, map[string]string{"item": ""}); got != nil {
		t.Fatalf("an absent field appearing empty is not a change, got %v", got)
	}
	if got, want := changedKeys(map[string]string{"pr": "12"}, map[string]string{}), []string{"pr"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a field disappearing is a change: got %v, want %v", got, want)
	}
}
