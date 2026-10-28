package panehost

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/agentpaths"
)

// A refresh request reaches a running host as a file in its spool directory
// followed by SIGUSR1: signals coalesce and carry no data, files do neither.
// The host drains the directory on the signal and sends one "refresh FIELD
// VALUE" (or "refresh manual") line per file, oldest first. A signal with an
// empty spool (an older CLI) is still "refresh manual".

// maxRefreshValue bounds a refresh value (one protocol line).
const maxRefreshValue = 1024

// reservedRefreshFields are sent by the host itself; the CLI may not forge
// them.
var reservedRefreshFields = map[string]bool{
	"manual": true, "session": true, "status": true, "title": true, "item": true, "pr": true,
	"asks": true,
}

// ValidRefreshField reports whether f may be sent with `pane refresh
// --field`: lowercase letters, digits, '-' and '_', starting with a letter,
// at most 32 characters, and not one of the fields the host sends itself.
func ValidRefreshField(f string) error {
	if f == "" || len(f) > 32 || f[0] < 'a' || f[0] > 'z' {
		return fmt.Errorf("field %q: want a lowercase name of at most 32 characters", f)
	}
	for _, c := range f {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return fmt.Errorf("field %q: only a-z, 0-9, '-' and '_'", f)
		}
	}
	if reservedRefreshFields[f] {
		return fmt.Errorf("field %q is sent by the host itself", f)
	}
	return nil
}

// RefreshValue keeps a value on one protocol line and within bounds.
func RefreshValue(v string) string {
	v = oneLine(v)
	if len(v) > maxRefreshValue {
		v = v[:maxRefreshValue]
		// Do not leave half a UTF-8 sequence at the cut.
		for len(v) > 0 && !utf8.ValidString(v) {
			v = v[:len(v)-1]
		}
	}
	return v
}

// SpoolDir is the refresh spool of the host process pid:
// <data dir>/runtime/panehost/<pid>.
func SpoolDir(pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("no host pid")
	}
	return agentpaths.EffectiveDataPath(filepath.Join("runtime", "panehost", strconv.Itoa(pid)), "runtime")
}

// PrepareSpool creates an empty spool directory (0700) for this host,
// replacing whatever a dead process with the same pid left.
func PrepareSpool(dir string) error {
	_ = os.RemoveAll(dir)
	return os.MkdirAll(dir, 0o700)
}

// Enqueue adds one refresh request to a host's spool: field "" means a
// manual refresh. It fails when the spool does not exist (no host, or a
// host too old to read it).
func Enqueue(dir, field, value string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	line := "manual"
	if field != "" {
		if err := ValidRefreshField(field); err != nil {
			return err
		}
		line = strings.TrimRight(field+" "+RefreshValue(value), " ")
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := fmt.Sprintf("%020d-%s", time.Now().UnixNano(), hex.EncodeToString(rnd[:]))
	tmp := filepath.Join(dir, "."+name)
	if err := os.WriteFile(tmp, []byte(line+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name+".req")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Drain removes and returns the queued requests, oldest first, each as the
// text after "refresh " ("manual" or "FIELD VALUE"). A malformed file is
// dropped.
func Drain(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".req") && e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		p := filepath.Join(dir, n)
		b, err := os.ReadFile(p)
		_ = os.Remove(p)
		if err != nil || len(b) > maxRefreshValue+64 {
			continue
		}
		line := strings.TrimSpace(string(b))
		field, value, _ := strings.Cut(line, " ")
		switch {
		case line == "manual":
			out = append(out, line)
		case ValidRefreshField(field) == nil:
			out = append(out, strings.TrimRight(field+" "+RefreshValue(value), " "))
		}
	}
	return out
}

// ListHostPanes lists every host pane on the server (all sessions).
func ListHostPanes(run Runner) ([]HostPane, error) {
	out, err := run("list-panes", "-a", "-F", "#{pane_id}|#{pane_pid}|#{"+optSlot+"}|#{"+optSession+"}")
	if err != nil {
		return nil, err
	}
	var panes []HostPane
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(l, "|", 4)
		if len(f) < 4 {
			continue
		}
		slot, _ := strconv.Atoi(f[2])
		if slot <= 0 {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		panes = append(panes, HostPane{PaneID: f[0], PID: pid, Slot: slot, SessionID: f[3]})
	}
	return panes, nil
}
