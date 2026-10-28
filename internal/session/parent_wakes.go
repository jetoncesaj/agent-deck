package session

import (
	"strings"
	"time"
)

// Parent wakes: which of a child's events may wake or message its parent.
//
// "all" (the default) is the upstream behaviour: every recorded turn reaches
// the parent's durable inbox, urgent turns wake it and info turns ride its
// next turn or the info digest, and the turn-start fleet snapshot lists the
// child.
//
// "escalation" keeps the parent link for what it means, who the child
// escalates to, and lets only an escalation reach the parent:
//
//	need     a NEED:/QUESTION:/ASK: line or a trailing question (Question);
//	         [inbox] question_wakes = false does not turn it off here
//	done     the child's completion sentinel, done or failed (Kind finished)
//	error    an error the child did not recover from within error_grace
//	         (a process that died at its prompt included)
//	stalled  no progress for escalate_after while nobody owes the child an
//	         answer: no open question to the parent, no running child of
//	         its own and no completion reported
//
// Everything else (running, waiting and idle flips, progress text, an error
// still inside its grace, a restart that resumed) stays in the turn journal,
// the events bus and `session children` / `list --json`, and is never written
// to the parent's inbox, so no drain, wake or digest can carry it, and the
// turn-start fleet snapshot leaves the child out. A reply to a tagged send
// still reaches its sender unless the sender is the parent: a parent that
// wants the answer to its own order waits for it (`session send --wait`).

const (
	ParentWakesAll        = "all"
	ParentWakesEscalation = "escalation"

	// DefaultEscalateAfter is [parent_wakes] escalate_after when unset.
	DefaultEscalateAfter = 30 * time.Minute
	// DefaultErrorGrace is [parent_wakes] error_grace when unset.
	DefaultErrorGrace = 3 * time.Minute
)

// Escalation kinds carried on TransitionNotificationEvent.Escalation and
// returned by ParentEscalation.
const (
	EscalationNeed    = "need"
	EscalationDone    = "done"
	EscalationError   = "error"
	EscalationStalled = "stalled"
)

// transitionKindEscalation marks a record the daemon raised for a child that
// stayed in error or stalled (escalation mode only); Escalation names which.
const transitionKindEscalation = "escalation"

// ParentWakesSettings is [parent_wakes], [profiles.<name>.parent_wakes] and
// [groups."<path>".parent_wakes]. Every key is optional; each one resolves on
// its own: the child's nearest group that sets it, else its profile, else the
// global section, else the default.
type ParentWakesSettings struct {
	// Mode is "all" (default) or "escalation". An unknown value reads as
	// "all", the upstream behaviour.
	Mode string `toml:"mode,omitempty"`
	// EscalateAfter is how long a child may show no progress, while nobody
	// owes it an answer, before its parent is woken once (escalation mode
	// only). A Go duration; "0" or "off" turns the time-out off. Default 30m.
	EscalateAfter string `toml:"escalate_after,omitempty"`
	// ErrorGrace is how long a child may stay in error before the error is
	// an escalation (escalation mode only). A Go duration; "0" escalates on
	// the first pass that sees the error. Default 3m.
	ErrorGrace string `toml:"error_grace,omitempty"`
}

// ParentWakePolicy is the resolved [parent_wakes] for one child.
type ParentWakePolicy struct {
	Mode          string
	EscalateAfter time.Duration
	ErrorGrace    time.Duration
}

// EscalationOnly reports whether only escalations may reach the parent.
func (p ParentWakePolicy) EscalationOnly() bool { return p.Mode == ParentWakesEscalation }

// ParentWakePolicyFor resolves [parent_wakes] for a child in profile and
// groupPath. A nil config yields the defaults (mode "all").
func (c *UserConfig) ParentWakePolicyFor(profile, groupPath string) ParentWakePolicy {
	var mode, after, grace string
	pick := func(s ParentWakesSettings) {
		if mode == "" {
			mode = strings.TrimSpace(s.Mode)
		}
		if after == "" {
			after = strings.TrimSpace(s.EscalateAfter)
		}
		if grace == "" {
			grace = strings.TrimSpace(s.ErrorGrace)
		}
	}
	if c != nil {
		for p := strings.TrimSpace(groupPath); p != "" && c.Groups != nil; p = getParentPath(p) {
			if g, ok := c.Groups[p]; ok {
				pick(g.ParentWakes)
			}
		}
		if ps, ok := c.Profiles[strings.TrimSpace(profile)]; ok {
			pick(ps.ParentWakes)
		}
		pick(c.ParentWakes)
	}
	return ParentWakePolicy{
		Mode:          normalizeParentWakesMode(mode),
		EscalateAfter: parseParentWakesDuration(after, DefaultEscalateAfter),
		ErrorGrace:    parseParentWakesDuration(grace, DefaultErrorGrace),
	}
}

func normalizeParentWakesMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), ParentWakesEscalation) {
		return ParentWakesEscalation
	}
	return ParentWakesAll
}

// parseParentWakesDuration reads a [parent_wakes] duration: "" is the
// default, "0" or "off" is 0 (off), an unparsable or negative value is the
// default.
func parseParentWakesDuration(v string, def time.Duration) time.Duration {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "":
		return def
	case "0", "off":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return def
	}
	return d
}

// parentWakePolicyOverride is a test seam: when set it replaces the loaded
// config for every child.
var parentWakePolicyOverride *ParentWakePolicy

// SetParentWakePolicyForTest forces the [parent_wakes] policy of every child
// for the current test; the returned func restores the previous one.
func SetParentWakePolicyForTest(p ParentWakePolicy) func() {
	prev := parentWakePolicyOverride
	parentWakePolicyOverride = &p
	return func() { parentWakePolicyOverride = prev }
}

// ResolveParentWakePolicy returns the [parent_wakes] policy for child, read
// from the loaded config.toml. profile is the profile the child was loaded
// from.
func ResolveParentWakePolicy(child *Instance, profile string) ParentWakePolicy {
	if parentWakePolicyOverride != nil {
		return *parentWakePolicyOverride
	}
	group := ""
	if child != nil {
		group = child.GroupPath
	}
	cfg, _ := LoadUserConfig()
	return cfg.ParentWakePolicyFor(profile, group)
}

// ParentEscalation reports why event may reach its parent when the child's
// parent wakes are "escalation", or "" when the event is routine and stays
// out of the parent's inbox. A completion sentinel (done or failed), a
// question to the parent, and the daemon's own error and stall records are
// escalations; a plain error flip, a restart that failed or a process that
// died at its prompt is not yet one (the daemon raises it after error_grace
// if the child stays in error, with the held record's text).
func ParentEscalation(event TransitionNotificationEvent) string {
	switch {
	case event.Kind == transitionKindFinished:
		return EscalationDone
	case strings.TrimSpace(event.Escalation) != "":
		return strings.TrimSpace(event.Escalation)
	case event.Question:
		return EscalationNeed
	}
	return ""
}

// questionEscalates reports whether a turn's question is one for the parent.
// In "all" mode every question is. In "escalation" mode a question in a turn
// the person started (they stepped into the child) is the person's to
// answer, unless the child printed an explicit NEED:/QUESTION:/ASK: line.
func questionEscalates(facts TurnFacts, p ParentWakePolicy) bool {
	if !facts.Question {
		return false
	}
	if !p.EscalationOnly() || facts.Trigger != TurnTriggerHuman {
		return true
	}
	return textHasNeedMarker(facts.Text)
}

// deliveryReasonRoutineHeld is commitEventToInbox's reason for a record it
// handled by keeping it out of the parent's inbox (committed=true).
const deliveryReasonRoutineHeld = "routine_held"

// noteHeldRoutine remembers the text of a held error record, so the error
// escalation the daemon raises later (if the child stays in error) says what
// went wrong instead of only that it did.
func (n *TransitionNotifier) noteHeldRoutine(event TransitionNotificationEvent) {
	if attentionClass(event.ToStatus) != "error" || strings.TrimSpace(event.Text) == "" {
		return
	}
	n.heldMu.Lock()
	defer n.heldMu.Unlock()
	if n.heldErrorText == nil {
		n.heldErrorText = map[string]string{}
	}
	n.heldErrorText[strings.TrimSpace(event.ChildSessionID)] = event.Text
}

// heldErrorTextOf returns the last held error text of child.
func (n *TransitionNotifier) heldErrorTextOf(child string) string {
	n.heldMu.Lock()
	defer n.heldMu.Unlock()
	return n.heldErrorText[child]
}

// forgetHeldErrorText drops child's held error text once its escalation is
// handled.
func (n *TransitionNotifier) forgetHeldErrorText(child string) {
	n.heldMu.Lock()
	defer n.heldMu.Unlock()
	delete(n.heldErrorText, child)
}

// NotifyEscalation commits one of the daemon's own escalation records (a
// child that stayed in error, or stalled) to the child's parent's durable
// inbox. Like NotifyFinished it has no from->to gate; the episode key in
// LastOutputHash makes it one record per episode, so a re-raise after a
// daemon restart is the same turn and is not written or woken again.
func (n *TransitionNotifier) NotifyEscalation(event TransitionNotificationEvent) (result TransitionNotificationEvent) {
	defer func() { publishTransitionEvent("session.escalation", result) }()

	event.Kind = transitionKindEscalation
	event.Profile = strings.TrimSpace(event.Profile)
	event.ChildTitle = strings.TrimSpace(event.ChildTitle)
	event.ChildSessionID = strings.TrimSpace(event.ChildSessionID)
	event.FromStatus = strings.ToLower(strings.TrimSpace(event.FromStatus))
	event.ToStatus = strings.ToLower(strings.TrimSpace(event.ToStatus))
	if event.Tier == "" {
		event.Tier = TurnTierUrgent
	}
	if event.Trigger == "" {
		event.Trigger = TurnTriggerSystem
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.ChildSessionID == "" || event.Profile == "" || strings.TrimSpace(event.Escalation) == "" {
		event.DeliveryResult = transitionDeliveryDropped
		return event
	}
	committed, transient, reason := n.commitEventToInbox(event)
	if committed {
		event.DeliveryResult = committedDeliveryResult(reason)
		return event
	}
	if transient {
		event.DeliveryResult = transitionDeliveryFailed
		return event
	}
	n.terminalDrop(event, reason)
	event.DeliveryResult = transitionDeliveryDropped
	event.DeadLetterReason = reason
	return event
}
