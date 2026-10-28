package tmux

import "testing"

// A quick Claude turn closes with "<spinner verb> for 0s" ("✻ Crunched for
// 0s", "✻ Cooked for 0s", "✻ Churned for 0s"). That is an ordinary finished
// turn, not a model outage. The classifier used to treat the literal
// "Crunched for 0s" as the model-unavailable no-op on its own, so a healthy
// session that answered in under a second read as error / model-unavailable
// (and, with escalation-only parent wakes, would wake its parent for nothing).
// Only the model's own "is currently unavailable" banner marks an outage.

const zeroSecondTurnPaneFooter = "──────────────────────────────────────────────────────────────── worker ─\n" +
	"❯ \n" +
	"──────────────────────────────────────────────────────────────────────────\n" +
	"  [profile] user@host:~/work/project | [Opus 5 (1M context)] ctx:4%\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents\n"

func zeroSecondTurnPane(summary string) string {
	return "❯ reply with only the word ok\n" +
		"\n" +
		"⏺ ok\n" +
		"\n" +
		summary + "\n" +
		"\n" +
		zeroSecondTurnPaneFooter
}

func TestClassifySubstate_ZeroSecondFinishedTurnIsNotModelUnavailable(t *testing.T) {
	d := NewPromptDetector("claude")
	for _, summary := range []string{
		"✻ Crunched for 0s",
		"✻ Crunched for 0s · done 9:12 AM",
		"✶ Crunched for 0s (0 tokens)",
		"✻ Cooked for 0s · done 4:13 PM",
		"✻ Churned for 0s · done 1:41 PM",
	} {
		t.Run(summary, func(t *testing.T) {
			pane := zeroSecondTurnPane(summary)
			if hasModelUnavailableNoop(pane) {
				t.Fatal("a finished zero-second turn is not the model-unavailable no-op")
			}
			if got := d.ClassifySubstate(pane); got != SubstateIdleAtEmptyPrompt {
				t.Fatalf("substate = %q, want %q", got, SubstateIdleAtEmptyPrompt)
			}
			if got := ClassifyPaneFrame("claude", pane); got != FrameWaiting {
				t.Fatalf("frame verdict = %q, want %q", got, FrameWaiting)
			}
		})
	}
}

func TestClassifySubstate_ModelUnavailableBannerWithZeroSecondSummary(t *testing.T) {
	d := NewPromptDetector("claude")
	pane := "❯ reply with only the word ok\n" +
		"\n" +
		"⏺ Fable is currently unavailable. Please try again later.\n" +
		"\n" +
		"✻ Crunched for 0s · done 9:12 AM\n" +
		"\n" +
		zeroSecondTurnPaneFooter
	if !hasModelUnavailableNoop(pane) {
		t.Fatal("the model's unavailable banner in the last turn is the no-op marker")
	}
	if got := d.ClassifySubstate(pane); got != SubstateModelUnavailable {
		t.Fatalf("substate = %q, want %q", got, SubstateModelUnavailable)
	}
	if got := ClassifyPaneFrame("claude", pane); got != FrameError {
		t.Fatalf("frame verdict = %q, want %q", got, FrameError)
	}
}
