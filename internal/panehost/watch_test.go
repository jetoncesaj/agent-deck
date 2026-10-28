package panehost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestParseWatch(t *testing.T) {
	cases := []struct {
		line    string
		ok, bad bool
		ids     []string
	}{
		{"watch a b c", true, false, []string{"a", "b", "c"}},
		{"watch a b a", true, false, []string{"a", "b"}},
		{"watch none", true, false, []string{}},
		{"watch", true, true, nil},
		{"watch a $(x)", true, true, nil},
		{"watch -t", true, true, nil},
		{"notify info x", false, false, nil},
		{"watcher a", false, false, nil},
	}
	for _, c := range cases {
		ids, ok, err := ParseWatch(c.line)
		if ok != c.ok || (err != nil) != c.bad || strings.Join(ids, ",") != strings.Join(c.ids, ",") {
			t.Errorf("ParseWatch(%q) = %q ok=%v err=%v", c.line, ids, ok, err)
		}
	}
	many := "watch"
	for i := 0; i <= MaxWatch; i++ {
		many += fmt.Sprintf(" s%d", i)
	}
	if _, ok, err := ParseWatch(many); !ok || err == nil {
		t.Fatalf("ParseWatch(%d ids) accepted", MaxWatch+1)
	}
}

func TestWatchStateChanges(t *testing.T) {
	var w watchState
	added := w.replace([]string{"a", "b"})
	if strings.Join(added, ",") != "a,b" {
		t.Fatalf("added = %q", added)
	}
	got := w.changes(map[string]string{"a": "running", "b": "error auth-401"}, added)
	if strings.Join(got, "|") != "refresh session a running|refresh session b error auth-401" {
		t.Fatalf("baseline = %q", got)
	}
	if got := w.changes(map[string]string{"a": "running", "b": "error auth-401"}, nil); len(got) != 0 {
		t.Fatalf("no change = %q", got)
	}
	// A failed read (id absent) changes nothing.
	if got := w.changes(map[string]string{"a": "waiting"}, nil); strings.Join(got, "|") != "refresh session a waiting" {
		t.Fatalf("change = %q", got)
	}
	// Replacing keeps what was sent for ids that stay; new ones are added.
	added = w.replace([]string{"b", "c"})
	if strings.Join(added, ",") != "c" {
		t.Fatalf("added on replace = %q", added)
	}
	if got := w.changes(map[string]string{"a": "idle", "b": "error auth-401", "c": "missing"}, nil); strings.Join(got, "|") != "refresh session c missing" {
		t.Fatalf("after replace = %q", got)
	}
	w.replace([]string{})
	if got := w.changes(map[string]string{"b": "running"}, nil); len(got) != 0 {
		t.Fatalf("after clear = %q", got)
	}
}

// The provider declares what it lists with a watch control line; the host
// sends each watched session's value at once, then only changes, read on
// the field tick.
func TestRunWatchFanOut(t *testing.T) {
	_, tty := testTTY(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.log")
	fifo := filepath.Join(dir, "ctl.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	var mu sync.Mutex
	statuses := map[string]string{"w1": "running", "w2": "waiting", "w3": "idle"}
	reads := 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, Options{
			// The provider copies whatever the test writes into the fifo to
			// its control fd.
			Command: "cat " + fifo + " >&3 & exec cat > " + in,
			TTY:     tty,
			Statuses: func(ids []string) map[string]string {
				mu.Lock()
				defer mu.Unlock()
				reads++
				out := map[string]string{}
				for _, id := range ids {
					if v, ok := statuses[id]; ok {
						out[id] = v
					} else {
						out[id] = "missing"
					}
				}
				return out
			},
			FieldsPeriod: 100 * time.Millisecond,
			Signals:      make(chan os.Signal),
			Grace:        300 * time.Millisecond,
		})
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, "hello", 3*time.Second, func() bool { return hasLine(in, "hello 2") })

	ctl, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	write := func(l string) {
		t.Helper()
		if _, err := fmt.Fprintln(ctl, l); err != nil {
			t.Fatal(err)
		}
	}

	write("watch w1 w2 gone")
	waitFor(t, "baseline", 3*time.Second, func() bool {
		return hasLine(in, "refresh session w1 running") && hasLine(in, "refresh session w2 waiting") && hasLine(in, "refresh session gone missing")
	})

	mu.Lock()
	statuses["w1"] = "waiting"
	statuses["w3"] = "error" // not watched
	mu.Unlock()
	waitFor(t, "change", 3*time.Second, func() bool { return hasLine(in, "refresh session w1 waiting") })

	// A flip and back between two ticks sends nothing.
	mu.Lock()
	statuses["w2"] = "running"
	statuses["w2"] = "waiting"
	mu.Unlock()
	time.Sleep(350 * time.Millisecond)
	if hasLine(in, "refresh session w2 running") || hasLine(in, "refresh session w3 error") {
		t.Fatalf("unexpected lines: %q", readLines(in))
	}

	// Replace the set, then clear it: no more lines, and no reads.
	write("watch w3")
	waitFor(t, "new member", 3*time.Second, func() bool { return hasLine(in, "refresh session w3 error") })
	write("watch none")
	time.Sleep(250 * time.Millisecond)
	mu.Lock()
	before := reads
	statuses["w3"] = "running"
	mu.Unlock()
	time.Sleep(350 * time.Millisecond)
	mu.Lock()
	after := reads
	mu.Unlock()
	if hasLine(in, "refresh session w3 running") || after != before {
		t.Fatalf("after watch none: lines %q, reads %d -> %d", readLines(in), before, after)
	}
	// A bad watch line is refused and leaves the set alone.
	write("watch a;b")
	time.Sleep(150 * time.Millisecond)
	if n := strings.Count(strings.Join(readLines(in), "\n"), "refresh session"); n != 5 {
		t.Fatalf("refresh session lines = %d, want 5: %q", n, readLines(in))
	}
}
