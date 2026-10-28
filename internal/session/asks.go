package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/atomicfile"
	"github.com/asheshgoplani/agent-deck/internal/events"
)

// "Asks you": a session declares that it needs the person (a decision, a
// pick, an approval or a fix) and the person steps into that session to
// answer. It is never relayed to the session's parent and is not an
// escalation: a session that also needs its parent prints a NEED: line.
//
// The state is set three ways and cleared three ways:
//
//	set    `agent-deck session ask <id|--self> --kind <k> "<question>"`
//	set    the marker line the agent prints, read from its finished turn
//	       by the notify daemon:
//	         ===AGENTDECK_ASKS=== kind=<k> [item=<ref>] question=<text>
//	clear  `agent-deck session ask <id|--self> --clear`, or the marker
//	       line ===AGENTDECK_ASKS=== clear
//	clear  a message from the person reaching the session (its
//	       UserPromptSubmit hook; see PromptIsFromPerson)
//
// One ask per session; a new one replaces the old. The record lives in
// runtime/asks/<id>.json; list/show JSON carry it as "asks", the side pane
// gets "refresh asks <kind>: <question>", the bus gets session.asks.set and
// session.asks.cleared, and the person gets one phone notification (web
// push) and one desktop notification per new ask.

// AsksMarker is the token of the line an agent prints to raise or clear its
// "asks you" state. It cannot be read as a NEED: line, and textAsksParent
// skips the line, so a question in it never wakes the parent.
const AsksMarker = "===AGENTDECK_ASKS==="

// Ask kinds.
const (
	AskKindDecision = "decision"
	AskKindPick     = "pick"
	AskKindApproval = "approval"
	AskKindFix      = "fix"
)

// AskKinds lists the valid kinds in display order.
var AskKinds = []string{AskKindDecision, AskKindPick, AskKindApproval, AskKindFix}

// Ask sources and clearers.
const (
	AskSourceCLI    = "cli"
	AskSourceMarker = "marker"

	AskClearedByPerson = "person"
	AskClearedByCLI    = "cli"
	AskClearedByMarker = "marker"
)

// maxAskQuestionRunes bounds the one-line question.
const maxAskQuestionRunes = 300

// PersonAsk is a session's open "asks you" state, as list/show JSON carry it.
type PersonAsk struct {
	Kind     string    `json:"kind"`
	Question string    `json:"question"`
	Since    time.Time `json:"since"`
	Item     string    `json:"item,omitempty"`
	Source   string    `json:"source,omitempty"`
}

// Key identifies one ask for deduplication (notifications).
func (a PersonAsk) Key() string {
	sum := sha256.Sum256([]byte(a.Kind + "\x00" + a.Question + "\x00" + a.Since.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])[:16]
}

// PaneValue is the side pane's "asks" field: "<kind>: <question>".
func (a *PersonAsk) PaneValue() string {
	if a == nil {
		return ""
	}
	return a.Kind + ": " + a.Question
}

// askRecord is the file. Ask is nil once cleared; the rest stays so a marker
// already applied is never applied again.
type askRecord struct {
	Ask        *PersonAsk `json:"ask,omitempty"`
	ClearedAt  time.Time  `json:"cleared_at,omitempty"`
	ClearedBy  string     `json:"cleared_by,omitempty"`
	MarkerTurn string     `json:"marker_turn,omitempty"`
}

func asksDir() string { return runtimeDirOrTemp("asks") }

func askPath(id string) string {
	return filepath.Join(asksDir(), sanitizeInboxName(id)+".json")
}

func loadAskRecord(id string) askRecord {
	var rec askRecord
	data, err := os.ReadFile(askPath(id)) // #nosec G304 -- sanitized id under the data dir
	if err != nil {
		return rec
	}
	_ = json.Unmarshal(data, &rec)
	return rec
}

func writeAskRecord(id string, rec askRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(asksDir(), 0o700); err != nil {
		return err
	}
	return atomicfile.WriteFile(askPath(id), data, 0o600)
}

// LoadAsk returns the session's open ask.
func LoadAsk(id string) (*PersonAsk, bool) {
	if strings.TrimSpace(id) == "" {
		return nil, false
	}
	rec := loadAskRecord(id)
	return rec.Ask, rec.Ask != nil
}

// OpenAsks returns every open ask by session id (one directory read).
func OpenAsks() map[string]*PersonAsk {
	entries, err := os.ReadDir(asksDir())
	if err != nil {
		return nil
	}
	out := map[string]*PersonAsk{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if a, ok := LoadAsk(id); ok {
			out[id] = a
		}
	}
	return out
}

// ValidAskKind reports whether kind is one of AskKinds.
func ValidAskKind(kind string) bool {
	for _, k := range AskKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// normalizeAskQuestion folds the question onto one line and bounds it.
func normalizeAskQuestion(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if utf8.RuneCountInString(q) > maxAskQuestionRunes {
		q = string([]rune(q)[:maxAskQuestionRunes-1]) + "…"
	}
	return q
}

// ErrAskInvalid is returned for an unknown kind or an empty question.
var ErrAskInvalid = errors.New("ask needs --kind decision|pick|approval|fix and a one-line question")

// SetAsk opens (or replaces) the session's ask. An open ask with the same
// kind, question and item is kept as it is, so a repeated set raises no
// second notification. changed reports a new or replaced ask.
func SetAsk(profile, id string, a PersonAsk) (_ PersonAsk, changed bool, err error) {
	a.Kind = strings.ToLower(strings.TrimSpace(a.Kind))
	a.Question = normalizeAskQuestion(a.Question)
	a.Item = strings.TrimSpace(a.Item)
	if !ValidAskKind(a.Kind) || a.Question == "" || strings.TrimSpace(id) == "" {
		return a, false, ErrAskInvalid
	}
	if a.Source == "" {
		a.Source = AskSourceCLI
	}
	withAskLock(id, func() {
		a, changed, err = setAskLocked(id, loadAskRecord(id), a)
	})
	if changed {
		publishAskSet(profile, id, a)
	}
	return a, changed, err
}

// setAskLocked opens a on rec and writes it; the caller holds the lock.
func setAskLocked(id string, rec askRecord, a PersonAsk) (PersonAsk, bool, error) {
	if cur := rec.Ask; cur != nil && cur.Kind == a.Kind && cur.Question == a.Question && cur.Item == a.Item {
		return *cur, false, nil
	}
	if a.Since.IsZero() {
		a.Since = time.Now().UTC()
	}
	rec.Ask = &a
	if err := writeAskRecord(id, rec); err != nil {
		return a, false, err
	}
	return a, true, nil
}

func publishAskSet(profile, id string, a PersonAsk) {
	publishAskEvent(profile, id, "session.asks.set", map[string]any{
		"kind": a.Kind, "question": a.Question, "since": a.Since, "item": a.Item, "source": a.Source,
	})
}

// ClearAsk closes the session's open ask. cleared is false when none was
// open (nothing is written then).
func ClearAsk(profile, id, by string) (cleared bool, err error) {
	var kind string
	withAskLock(id, func() {
		kind, cleared, err = clearAskLocked(id, loadAskRecord(id), by)
	})
	if cleared {
		publishAskCleared(profile, id, kind, by)
	}
	return cleared, err
}

// clearAskLocked closes rec's ask and writes it; the caller holds the lock.
func clearAskLocked(id string, rec askRecord, by string) (kind string, cleared bool, err error) {
	if rec.Ask == nil {
		return "", false, nil
	}
	kind = rec.Ask.Kind
	rec.Ask = nil
	rec.ClearedAt = time.Now().UTC()
	rec.ClearedBy = by
	if err := writeAskRecord(id, rec); err != nil {
		return kind, false, err
	}
	forgetAskNotified(id)
	return kind, true, nil
}

func publishAskCleared(profile, id, kind, by string) {
	publishAskEvent(profile, id, "session.asks.cleared", map[string]any{"kind": kind, "by": by})
}

// askLocks serializes ask updates within this process; the sibling flock
// serializes them across processes (the prompt hook's clear, the daemon's
// marker, the CLI), so none overwrites another's write.
var askLocks sync.Mutex

func askLockPath(id string) string { return askPath(id) + ".lock" }

func withAskLock(id string, fn func()) {
	askLocks.Lock()
	defer askLocks.Unlock()
	if err := os.MkdirAll(asksDir(), 0o700); err == nil {
		f, err := os.OpenFile(askLockPath(id), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- sanitized id under the data dir
		if err == nil {
			defer func() { _ = f.Close() }()
			if syscall.Flock(int(f.Fd()), syscall.LOCK_EX) == nil {
				defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
			}
		}
	}
	fn()
}

// RemoveAsk deletes a removed session's ask state.
func RemoveAsk(id string) {
	_ = os.Remove(askPath(id))
	_ = os.Remove(askLockPath(id))
	_ = os.Remove(askRelayPath(id))
	forgetAskNotified(id)
}

// publishAskFrame is events.PublishProfile; tests capture the frames.
var publishAskFrame = events.PublishProfile

func publishAskEvent(profile, id, kind string, data map[string]any) {
	publishAskFrame(profile, kind, id, data)
	sessionLog.Debug("asks_"+strings.TrimPrefix(kind, "session.asks."),
		slog.String("session", id), slog.Any("kind", data["kind"]))
}

// --- the marker line ---------------------------------------------------------

// AsksMarkerLine is a parsed marker: an ask, or Clear.
type AsksMarkerLine struct {
	Ask   PersonAsk
	Clear bool
}

// ParseAsksMarker parses one line:
//
//	===AGENTDECK_ASKS=== kind=<k> [item=<ref>] question=<text to end of line>
//	===AGENTDECK_ASKS=== clear
//
// The marker must start the line (markdown emphasis and bullets ignored), so
// a line that quotes it in prose is not a marker. An unknown kind or an
// empty question is rejected.
func ParseAsksMarker(line string) (AsksMarkerLine, bool) {
	rest, ok := strings.CutPrefix(strings.TrimLeft(strings.TrimSpace(line), "-*>#•· \t_`"), AsksMarker)
	if !ok {
		return AsksMarkerLine{}, false
	}
	rest = strings.TrimSpace(strings.TrimRight(rest, "*_`"))
	if rest == "clear" {
		return AsksMarkerLine{Clear: true}, true
	}
	head, question, found := strings.Cut(rest, "question=")
	if !found {
		return AsksMarkerLine{}, false
	}
	var m AsksMarkerLine
	for _, f := range strings.Fields(head) {
		switch k, v, _ := strings.Cut(f, "="); k {
		case "kind":
			m.Ask.Kind = strings.ToLower(v)
		case "item":
			m.Ask.Item = v
		}
	}
	m.Ask.Question = normalizeAskQuestion(question)
	if !ValidAskKind(m.Ask.Kind) || m.Ask.Question == "" {
		return AsksMarkerLine{}, false
	}
	return m, true
}

// ScanAsksMarker returns the last marker line of text.
func ScanAsksMarker(text string) (AsksMarkerLine, bool) {
	var found AsksMarkerLine
	var ok bool
	for line := range strings.SplitSeq(text, "\n") {
		if m, valid := ParseAsksMarker(line); valid {
			found, ok = m, true
		}
	}
	return found, ok
}

// lineIsAsksMarker reports a marker line (valid or not), which the question
// rule skips.
func lineIsAsksMarker(line string) bool {
	return strings.HasPrefix(strings.TrimLeft(strings.TrimSpace(line), "-*>#•· \t_`"), AsksMarker)
}

// ApplyAsksMarker applies the marker of a finished turn once: turn is its
// transcript identity, so the daemon seeing the same turn again (a hook
// re-fire, a status flip) does not raise an ask the person already cleared.
// It reports whether the ask changed.
func ApplyAsksMarker(profile, id, turn, text string) bool {
	m, ok := ScanAsksMarker(text)
	if !ok || strings.TrimSpace(turn) == "" {
		return false
	}
	m.Ask.Source = AskSourceMarker
	var changed bool
	var kind string
	withAskLock(id, func() {
		rec := loadAskRecord(id)
		if rec.MarkerTurn == turn {
			return
		}
		rec.MarkerTurn = turn
		if err := writeAskRecord(id, rec); err != nil {
			return
		}
		if m.Clear {
			kind, changed, _ = clearAskLocked(id, rec, AskClearedByMarker)
			return
		}
		var err error
		m.Ask, changed, err = setAskLocked(id, rec, m.Ask)
		changed = changed && err == nil
	})
	switch {
	case !changed:
	case m.Clear:
		publishAskCleared(profile, id, kind, AskClearedByMarker)
	default:
		publishAskSet(profile, id, m.Ask)
	}
	return changed
}

// --- the person versus agent-deck relays --------------------------------------

// PromptIsFromPerson reports whether a prompt that reached a session came
// from the person rather than from agent-deck or another session:
//
//   - a tagged send ([agent-deck from:<id>]) is another session's;
//   - an inbox wake, a heartbeat, Stop-hook feedback, a task notification,
//     a system reminder or command output is agent-deck's or the harness's;
//   - a bare slash command is not an answer;
//   - an untagged send that another session made while the ask was open
//     (an untagged Codex sender, --no-tag, tag_sends = false) is recorded
//     by `session send` as a relay and matched here by its text.
//
// Everything else is the person: typed into the pane (TUI, web, phone
// terminal), or sent from a plain shell or a bridge acting for them.
func PromptIsFromPerson(id, prompt string) bool {
	text := strings.TrimSpace(prompt)
	switch {
	case text == "",
		strings.HasPrefix(text, sendEnvelopePrefix),
		hasInboxWakePrefix(text),
		IsConductorHeartbeatMessage(text),
		strings.HasPrefix(text, "Stop hook feedback:"),
		strings.HasPrefix(text, "<task-notification>"),
		strings.HasPrefix(text, "<system-reminder>"),
		strings.HasPrefix(text, "<local-command"),
		strings.HasPrefix(text, "<command-name>"),
		strings.HasPrefix(text, "/") && !strings.ContainsAny(text, " \n"):
		return false
	}
	return !askRelayMatches(id, text)
}

// askRelayTTL bounds how long a recorded relay matches a prompt.
const askRelayTTL = 10 * time.Minute

type askRelay struct {
	Hash   string    `json:"hash"`
	Sender string    `json:"sender"`
	At     time.Time `json:"at"`
}

func askRelayPath(id string) string {
	return filepath.Join(asksDir(), sanitizeInboxName(id)+".relay")
}

func askRelayHash(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:])[:16]
}

// RecordAskRelay notes that sender, another session, sent message to id
// while id has an open ask, so id's prompt hook does not read it as the
// person's answer. A no-op when id has no open ask or sender is empty.
func RecordAskRelay(id, sender, message string) {
	sender = strings.TrimSpace(sender)
	if sender == "" || sender == id {
		return
	}
	if _, open := LoadAsk(id); !open {
		return
	}
	now := time.Now().UTC()
	relays := []askRelay{{Hash: askRelayHash(message), Sender: sender, At: now}}
	for _, r := range loadAskRelays(id) {
		if now.Sub(r.At) < askRelayTTL && len(relays) < 16 {
			relays = append(relays, r)
		}
	}
	if data, err := json.Marshal(relays); err == nil {
		_ = atomicfile.WriteFile(askRelayPath(id), data, 0o600)
	}
}

func loadAskRelays(id string) []askRelay {
	data, err := os.ReadFile(askRelayPath(id)) // #nosec G304 -- sanitized id under the data dir
	if err != nil {
		return nil
	}
	var relays []askRelay
	_ = json.Unmarshal(data, &relays)
	return relays
}

func askRelayMatches(id, text string) bool {
	hash := askRelayHash(text)
	now := time.Now()
	for _, r := range loadAskRelays(id) {
		if r.Hash == hash && now.Sub(r.At) < askRelayTTL {
			return true
		}
	}
	return false
}

// ClearAskOnPrompt is the prompt hook's rule: an open ask closes when the
// person's message reaches the session.
func ClearAskOnPrompt(profile, id, prompt string) bool {
	if _, open := LoadAsk(id); !open || !PromptIsFromPerson(id, prompt) {
		return false
	}
	cleared, err := ClearAsk(profile, id, AskClearedByPerson)
	return err == nil && cleared
}

// --- notifications -------------------------------------------------------------

func askNotifiedDir() string { return filepath.Join(asksDir(), "notified") }

// MarkAskNotified claims the one notification of ask a on channel ("push",
// "desktop"): true exactly once per ask across processes and restarts.
func MarkAskNotified(id string, a *PersonAsk, channel string) bool {
	if a == nil {
		return false
	}
	if err := os.MkdirAll(askNotifiedDir(), 0o700); err != nil {
		return false
	}
	name := fmt.Sprintf("%s.%s.%s", sanitizeInboxName(id), channel, a.Key())
	f, err := os.OpenFile(filepath.Join(askNotifiedDir(), name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- sanitized under the data dir
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func forgetAskNotified(id string) {
	matches, _ := filepath.Glob(filepath.Join(askNotifiedDir(), sanitizeInboxName(id)+".*"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// AskNotificationText is the title and body of an ask's notification.
func AskNotificationText(title string, a *PersonAsk) (string, string) {
	if strings.TrimSpace(title) == "" {
		title = "agent-deck"
	}
	body := "asks you (" + a.Kind + "): " + a.Question
	if a.Item != "" {
		body += " [" + a.Item + "]"
	}
	return title, body
}
