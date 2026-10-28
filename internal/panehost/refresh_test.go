package panehost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestValidRefreshField(t *testing.T) {
	for _, ok := range []string{"plane", "item-state", "x_1", "a"} {
		if err := ValidRefreshField(ok); err != nil {
			t.Errorf("ValidRefreshField(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Plane", "1x", "a b", "a;b", "-x", strings.Repeat("a", 33),
		"manual", "session", "status", "title", "item", "pr"} {
		if err := ValidRefreshField(bad); err == nil {
			t.Errorf("ValidRefreshField(%q) accepted", bad)
		}
	}
}

func TestRefreshValueOneLineAndBounded(t *testing.T) {
	if got := RefreshValue("  OPS-9 \n moved\tto  review "); got != "OPS-9 moved to review" {
		t.Fatalf("RefreshValue = %q", got)
	}
	long := strings.Repeat("é", maxRefreshValue) // 2 bytes each
	got := RefreshValue(long)
	if len(got) > maxRefreshValue || !strings.HasPrefix(long, got) || len(got)%2 != 0 {
		t.Fatalf("RefreshValue(long) = %d bytes, cut mid-rune or too long", len(got))
	}
}

func TestSpoolEnqueueDrain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	if err := Enqueue(dir, "plane", "x"); err == nil {
		t.Fatal("Enqueue into a missing spool succeeded")
	}
	if err := PrepareSpool(dir); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("spool mode = %v", st.Mode().Perm())
	}
	if err := Enqueue(dir, "plane", "OPS-9 in review"); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(dir, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(dir, "plane", ""); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(dir, "status", "running"); err == nil {
		t.Fatal("Enqueue of a host field succeeded")
	}
	// A forged file with a host field is dropped on drain.
	if err := os.WriteFile(filepath.Join(dir, "99999999999999999999-x.req"), []byte("status waiting\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := Drain(dir)
	want := []string{"plane OPS-9 in review", "manual", "plane"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("Drain = %q, want %q", got, want)
	}
	if again := Drain(dir); len(again) != 0 {
		t.Fatalf("second Drain = %q", again)
	}
	// A spool left by a dead process with the same pid starts empty.
	if err := Enqueue(dir, "plane", "stale"); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSpool(dir); err != nil {
		t.Fatal(err)
	}
	if got := Drain(dir); len(got) != 0 {
		t.Fatalf("Drain after PrepareSpool = %q", got)
	}
}

func TestRunRefreshLinesFromSpool(t *testing.T) {
	_, tty := testTTY(t)
	in := filepath.Join(t.TempDir(), "in.log")
	spool := filepath.Join(t.TempDir(), "spool")
	if err := PrepareSpool(spool); err != nil {
		t.Fatal(err)
	}
	sigs := make(chan os.Signal, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, Options{
			Command:   "exec cat > " + in,
			TTY:       tty,
			Refreshes: func() []string { return Drain(spool) },
			Signals:   sigs,
			Grace:     300 * time.Millisecond,
		})
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, "hello", 3*time.Second, func() bool { return hasLine(in, "hello 2") })

	// Two requests, one (coalesced) signal: both lines arrive, in order.
	if err := Enqueue(spool, "plane", "OPS-9 review"); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(spool, "plane", "OPS-10 done"); err != nil {
		t.Fatal(err)
	}
	sigs <- syscall.SIGUSR1
	waitFor(t, "field lines", 3*time.Second, func() bool {
		l := readLines(in)
		return len(l) >= 4 && l[len(l)-2] == "refresh plane OPS-9 review" && l[len(l)-1] == "refresh plane OPS-10 done"
	})
	if hasLine(in, "refresh manual") {
		t.Fatal("a field refresh also sent refresh manual")
	}
	// A bare signal (an older CLI) is still a manual refresh.
	sigs <- syscall.SIGUSR1
	waitFor(t, "manual", 3*time.Second, func() bool { return hasLine(in, "refresh manual") })
}
