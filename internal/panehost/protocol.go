// Package panehost runs a provider command in a side pane beside an Agent
// Deck session and owns that pane's life: input forwarding, refresh signals,
// layout and teardown.
//
// The host is generic: it knows no tracker, code host or project. A provider
// is any command. It draws on the pane's terminal (its stdout and stderr are
// the pane) and reads one event per line on stdin:
//
//	hello 1                 first line; 1 is the protocol version
//	resize COLS ROWS        pane size, sent at start and on every change
//	click X Y               left button press, 0-based cell in the pane
//	rclick X Y              right button press
//	mclick X Y              middle button press
//	scroll up X Y           wheel up over cell X Y
//	scroll down X Y         wheel down over cell X Y
//	key NAME                a key typed while the pane had focus: one
//	                        printable character, or enter, tab, backspace,
//	                        escape, space, up, down, left, right, home, end,
//	                        pgup, pgdn, delete, C-a .. C-z
//	refresh FIELD VALUE     the session's FIELD (status, title, item, pr)
//	                        changed to VALUE (VALUE may be empty)
//	refresh manual          someone asked for a refresh (agent-deck pane refresh)
//	attach                  a client attached: resend anything the terminal
//	                        does not keep, such as kitty graphics
//
// End of file on stdin means the host is going away: exit. Unknown lines must
// be ignored, so the host can add events without breaking providers.
//
// A provider may write control lines to the file descriptor named by
// AGENTDECK_PANE_CONTROL_FD (3):
//
//	notify TEXT             raise a notification (tier urgent)
//	notify urgent TEXT      the same, tier explicit
//	notify info TEXT        a low-priority notification (digest tier)
package panehost

import (
	"fmt"
	"strconv"
)

// ProtocolVersion is sent in the hello line.
const ProtocolVersion = 1

// Mouse tracking: button press/release reporting (1000) in SGR encoding
// (1006), so coordinates beyond column 223 survive.
const (
	mouseOn  = "\x1b[?1000h\x1b[?1006h"
	mouseOff = "\x1b[?1000l\x1b[?1006l"
)

// InputParser turns the raw bytes a terminal sends to the pane (keys and SGR
// mouse reports) into protocol lines. It keeps an incomplete escape sequence
// between Feed calls.
type InputParser struct {
	pending []byte
}

// Feed consumes b and returns the complete events it held, as protocol lines
// without the trailing newline.
func (p *InputParser) Feed(b []byte) []string {
	buf := append(p.pending, b...)
	p.pending = nil
	var out []string
	for i := 0; i < len(buf); {
		c := buf[i]
		if c != 0x1b {
			if line := keyLine(c); line != "" {
				out = append(out, line)
			}
			i++
			continue
		}
		// Escape sequence.
		if i+1 >= len(buf) {
			// A lone ESC at the end of a read is the escape key: terminals
			// and tmux write a whole sequence in one write.
			out = append(out, "key escape")
			i++
			continue
		}
		next := buf[i+1]
		switch next {
		case '[':
			n, line, complete := parseCSI(buf[i:])
			if !complete {
				p.pending = append([]byte(nil), buf[i:]...)
				return out
			}
			if line != "" {
				out = append(out, line)
			}
			i += n
		case 'O':
			if i+2 >= len(buf) {
				p.pending = append([]byte(nil), buf[i:]...)
				return out
			}
			if name := ss3Key(buf[i+2]); name != "" {
				out = append(out, "key "+name)
			}
			i += 3
		default:
			// ESC followed by something else: escape, then the next byte
			// on its own (an Alt-modified key is reported as both).
			out = append(out, "key escape")
			i++
		}
	}
	return out
}

// parseCSI parses one CSI sequence at the start of b (b[0]=ESC, b[1]='[').
// It returns the bytes consumed, the protocol line ("" for sequences with no
// event), and false when the sequence is not complete yet.
func parseCSI(b []byte) (n int, line string, complete bool) {
	// Find the final byte (0x40..0x7e) after parameter/intermediate bytes.
	j := 2
	for ; j < len(b); j++ {
		if b[j] >= 0x40 && b[j] <= 0x7e {
			break
		}
		if j-2 > 64 {
			// Garbage: drop the introducer and resync.
			return 2, "", true
		}
	}
	if j >= len(b) {
		return 0, "", false
	}
	params := string(b[2:j])
	final := b[j]
	n = j + 1
	if len(params) > 0 && params[0] == '<' && (final == 'M' || final == 'm') {
		return n, sgrMouseLine(params[1:], final == 'M'), true
	}
	switch final {
	case 'A':
		return n, "key up", true
	case 'B':
		return n, "key down", true
	case 'C':
		return n, "key right", true
	case 'D':
		return n, "key left", true
	case 'H':
		return n, "key home", true
	case 'F':
		return n, "key end", true
	case 'Z':
		return n, "key S-tab", true
	case '~':
		switch params {
		case "1", "7":
			return n, "key home", true
		case "4", "8":
			return n, "key end", true
		case "3":
			return n, "key delete", true
		case "5":
			return n, "key pgup", true
		case "6":
			return n, "key pgdn", true
		}
	}
	return n, "", true
}

// sgrMouseLine converts SGR mouse parameters "b;x;y" (1-based cells) into a
// protocol line. Releases and motion produce no event.
func sgrMouseLine(params string, press bool) string {
	var nums [3]int
	k := 0
	start := 0
	for i := 0; i <= len(params); i++ {
		if i == len(params) || params[i] == ';' {
			if k >= 3 {
				return ""
			}
			v, err := strconv.Atoi(params[start:i])
			if err != nil {
				return ""
			}
			nums[k] = v
			k++
			start = i + 1
		}
	}
	if k != 3 || !press {
		return ""
	}
	btn, x, y := nums[0], nums[1]-1, nums[2]-1
	if x < 0 || y < 0 {
		return ""
	}
	if btn&32 != 0 {
		return "" // motion
	}
	base := btn &^ (4 | 8 | 16) // drop shift/meta/ctrl
	switch base {
	case 0:
		return fmt.Sprintf("click %d %d", x, y)
	case 1:
		return fmt.Sprintf("mclick %d %d", x, y)
	case 2:
		return fmt.Sprintf("rclick %d %d", x, y)
	case 64:
		return fmt.Sprintf("scroll up %d %d", x, y)
	case 65:
		return fmt.Sprintf("scroll down %d %d", x, y)
	}
	return ""
}

func ss3Key(c byte) string {
	switch c {
	case 'A':
		return "up"
	case 'B':
		return "down"
	case 'C':
		return "right"
	case 'D':
		return "left"
	case 'H':
		return "home"
	case 'F':
		return "end"
	}
	return ""
}

func keyLine(c byte) string {
	switch {
	case c == '\r' || c == '\n':
		return "key enter"
	case c == '\t':
		return "key tab"
	case c == 0x7f || c == 0x08:
		return "key backspace"
	case c == ' ':
		return "key space"
	case c >= 1 && c <= 26:
		return "key C-" + string(rune('a'+c-1))
	case c >= 0x21 && c < 0x7f:
		return "key " + string(rune(c))
	case c >= 0x80:
		// UTF-8 text is not forwarded byte by byte; side panes take
		// navigation keys, not prose.
		return ""
	}
	return ""
}
