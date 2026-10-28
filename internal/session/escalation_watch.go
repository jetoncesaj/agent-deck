package session

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// The daemon's half of [parent_wakes] mode = "escalation" (parent_wakes.go):
// the two escalations no single turn carries. Each pass looks at every
// parented child in escalation mode and raises, once per episode:
//
//   - error: the child has been in error for error_grace (it did not
//     recover). A child that leaves error within the grace raises nothing.
//   - stalled: the child (running, waiting or idle) has shown no progress
//     for escalate_after, nobody owes it an answer (its last turn asked its
//     parent no question, no child of its own is running) and it has not
//     reported done.
//
// Progress is any change of status, transcript size or modification time, or
// hook status time. The episode key is built from those durable signals, not
// from daemon memory, so a daemon restart re-raises the same episode under the
// same turn fingerprint and the inbox's exactly-once rules keep it one record
// and one wake.

// escalationWatch is the daemon's per-child memory for the sweep.
type escalationWatch struct {
	progressKey string    // status + durable progress stamp, last pass
	progressAt  time.Time // when progressKey last changed
	errorSince  time.Time // first pass seen in error; zero when not in error
	errorKey    string    // progress stamp when the error began
	sent        string    // episode key of the last escalation raised
}

// progressStamp is the durable progress signal of inst: transcript size and
// mtime, and the hook status time. at is the newest of the times, zero when
// none is known.
func progressStamp(inst *Instance, hs *HookStatus) (key string, at time.Time) {
	var parts []string
	if path := inst.GetJSONLPath(); path != "" {
		if info, err := os.Stat(path); err == nil {
			parts = append(parts, fmt.Sprintf("jsonl:%d:%d", info.Size(), info.ModTime().UnixNano()))
			at = info.ModTime()
		}
	}
	if hs != nil && !hs.UpdatedAt.IsZero() {
		parts = append(parts, fmt.Sprintf("hook:%d", hs.UpdatedAt.UnixNano()))
		if hs.UpdatedAt.After(at) {
			at = hs.UpdatedAt
		}
	}
	return strings.Join(parts, "|"), at
}

// sweepEscalations raises the error and stall escalations for this pass.
func (d *TransitionDaemon) sweepEscalations(profile string, byID map[string]*Instance, statuses map[string]string, hookStatuses map[string]*HookStatus, notifyEnabled bool, now time.Time) {
	if d.escalations == nil {
		d.escalations = map[string]map[string]*escalationWatch{}
	}
	watches := d.escalations[profile]
	if watches == nil {
		watches = map[string]*escalationWatch{}
		d.escalations[profile] = watches
	}
	for id := range watches {
		if byID[id] == nil {
			delete(watches, id)
		}
	}
	for id, inst := range byID {
		if !notifyEnabled || !escalationCandidate(inst) {
			delete(watches, id)
			continue
		}
		pol := ResolveParentWakePolicy(inst, profile)
		if !pol.EscalationOnly() {
			delete(watches, id)
			continue
		}
		status := normalizeStatusString(statuses[id])
		if status != string(StatusRunning) && status != string(StatusWaiting) &&
			status != string(StatusIdle) && status != string(StatusError) {
			delete(watches, id) // stopped, queued, starting: nothing to watch
			continue
		}
		stamp, stampAt := progressStamp(inst, hookStatuses[id])
		key := status + "|" + stamp
		w := watches[id]
		if w == nil {
			// First sight (or a daemon restart): the durable stamp says when
			// the child last moved.
			w = &escalationWatch{progressKey: key, progressAt: stampAt}
			if w.progressAt.IsZero() {
				w.progressAt = now
			}
			watches[id] = w
		} else if key != w.progressKey {
			w.progressKey = key
			w.progressAt = now
		}

		if status == string(StatusError) {
			if w.errorSince.IsZero() {
				w.errorSince, w.errorKey = now, stamp
			}
			episode := "error:" + w.errorKey
			if now.Sub(w.errorSince) >= pol.ErrorGrace && w.sent != episode {
				if d.raiseEscalation(profile, inst, EscalationError, episode, status, now,
					errorEscalationText(now.Sub(w.errorSince), d.notifier.heldErrorTextOf(id))) {
					w.sent = episode
					d.notifier.forgetHeldErrorText(id)
				}
			}
			continue
		}
		if !w.errorSince.IsZero() {
			// Recovered within the grace (or after it): a later error
			// starts a new episode with its own text.
			d.notifier.forgetHeldErrorText(id)
			w.errorSince, w.errorKey = time.Time{}, ""
		}

		if pol.EscalateAfter <= 0 || now.Sub(w.progressAt) < pol.EscalateAfter {
			continue
		}
		episode := "stalled:" + w.progressKey
		if w.sent == episode || childOwedAnswerOrDone(inst, byID, statuses) {
			continue
		}
		if d.raiseEscalation(profile, inst, EscalationStalled, episode, status, now,
			fmt.Sprintf("No progress for %s (status %s) and nobody owes it an answer.", now.Sub(w.progressAt).Round(time.Minute), status)) {
			w.sent = episode
		}
	}
}

// escalationCandidate reports whether inst has a parent to escalate to.
func escalationCandidate(inst *Instance) bool {
	if inst == nil || !instanceAcceptsTransitionEvents(inst) || isSelfSuppressedConductor(inst) {
		return false
	}
	parent := strings.TrimSpace(inst.ParentSessionID)
	return parent != "" && parent != inst.ID
}

// childOwedAnswerOrDone reports whether a stall must not be raised: its last
// turn asked the parent a question (already an escalation) or reported done,
// it asks the person something (session ask), or a child of its own is
// running (an Epic conductor waiting on its workers is owed their results,
// not stalled).
func childOwedAnswerOrDone(inst *Instance, byID map[string]*Instance, statuses map[string]string) bool {
	if _, open := LoadAsk(inst.ID); open {
		return true
	}
	if last := LastTurnJournalEntry(inst.ID); last != nil && (last.Question || last.DoneStatus != "") {
		return true
	}
	for id, other := range byID {
		if other == nil || strings.TrimSpace(other.ParentSessionID) != inst.ID || id == inst.ID {
			continue
		}
		switch normalizeStatusString(statuses[id]) {
		case string(StatusRunning), string(StatusStarting):
			return true
		}
	}
	return false
}

func errorEscalationText(held time.Duration, detail string) string {
	text := fmt.Sprintf("In error for %s and did not recover.", held.Round(time.Second))
	if detail = strings.TrimSpace(detail); detail != "" {
		text += "\n" + detail
	}
	return text
}

// raiseEscalation commits one escalation record. It reports whether the
// record is handled (committed, or terminally undeliverable); a transient
// failure is retried on the next pass.
func (d *TransitionDaemon) raiseEscalation(profile string, inst *Instance, kind, episode, status string, now time.Time, text string) bool {
	event := TransitionNotificationEvent{
		ChildSessionID: inst.ID,
		ChildTitle:     inst.Title,
		Profile:        profile,
		FromStatus:     status,
		ToStatus:       status,
		Timestamp:      now,
		Substate:       string(inst.CachedSubstate()),
		ExitCode:       inst.ExitCode(),
		Escalation:     kind,
		Text:           CapTurnText(text, ResolveInboxConfigForInstance(nil).GetMaxTextBytes()),
		LastOutputHash: episode,
	}
	result := d.notifier.NotifyEscalation(event)
	if result.DeliveryResult == transitionDeliveryCommitted {
		_ = BumpInboxStats(statsParentFor(inst), func(s *InboxStats) { s.Escalations++ })
	}
	return result.DeliveryResult != transitionDeliveryFailed
}
