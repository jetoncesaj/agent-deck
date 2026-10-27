package panehost

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/testutil"
)

// hostHelperEnv makes the test binary act as the pane host inside a tmux
// pane of a private test server (see openTestHost). The pane inherits the
// isolated TMUX_TMPDIR and HOME from that server, so the helper must not
// isolate again.
const hostHelperEnv = "PANEHOST_TEST_HOST"

func TestMain(m *testing.M) {
	if os.Getenv(hostHelperEnv) == "1" {
		os.Exit(runTestHost())
	}
	cleanupHome := testutil.IsolateHome()
	cleanupTmux := testutil.IsolateTmuxSocket()
	code := m.Run()
	cleanupTmux()
	cleanupHome()
	os.Exit(code)
}

// runTestHost is the in-pane host for the tmux tests: the same wiring the
// CLI's `pane run` does, minus Agent Deck storage.
func runTestHost() int {
	socket := os.Getenv("PANEHOST_TEST_SOCKET")
	session := os.Getenv("PANEHOST_TEST_SESSION")
	agent := os.Getenv("PANEHOST_TEST_AGENT")
	self := os.Getenv("TMUX_PANE")
	// Inside its own private test server, $TMUX names the very socket the
	// helper targets with -L, which the default-socket guard reads as "the
	// server this test was launched from" and refuses. The helper always
	// passes -L, so $TMUX is not needed.
	_ = os.Unsetenv("TMUX")
	run := SocketRunner(socket)
	ctlLog := os.Getenv("PANEHOST_TEST_CONTROL_LOG")
	code, err := Run(context.Background(), Options{
		Command: os.Getenv("PANEHOST_TEST_CMD"),
		Env:     append(os.Environ(), "AGENTDECK_PANE_CONTROL_FD=3"),
		TTY:     os.Stdin,
		Probe: func() Probe {
			p := ProbeAgent(run, agent)
			if l := os.Getenv("PANEHOST_TEST_PROBE_LOG"); l != "" {
				if f, err := os.OpenFile(l, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
					fmt.Fprintf(f, "%+v\n", p)
					_ = f.Close()
				}
			}
			return p
		},
		ProbePeriod: 100 * time.Millisecond,
		Layout:      func() { ApplyLayoutAs(run, session, self) },
		Grace:       500 * time.Millisecond,
		OnControl: func(line string) {
			if ctlLog == "" {
				return
			}
			f, err := os.OpenFile(ctlLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				fmt.Fprintln(f, line)
				_ = f.Close()
			}
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	Cleanup(run, session, self)
	return code
}
