package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The session log: one JSON object per line, everything the harness saw and
// decided in a session, so a run can be brought back and analysed after the
// fact. It records decisions, not the whole context: what the harness added
// to each request (ledger, handoff, riders), the replies, tool results
// (clipped), every harness correction, every line put on the screen. Never
// API keys: those live in headers, which are not logged.

type SessionLog struct {
	mu   sync.Mutex
	f    *os.File
	Path string
}

// logDir is where session logs go: LCHAT_LOG_DIR, else the XDG state dir,
// else ~/.local/state/lchat/sessions. "" when logging is off.
func logDir() string {
	switch getenv("LCHAT_LOG") {
	case "off", "0", "false", "no":
		return ""
	}
	if d := getenv("LCHAT_LOG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "lchat", "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "lchat", "sessions")
}

// OpenSessionLog creates this session's file. The directory is private and so
// is the file: tool outputs can contain anything the model read.
func OpenSessionLog(dir string) (*SessionLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := time.Now().Format("2006-01-02T15-04-05") + ".jsonl"
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &SessionLog{f: f, Path: f.Name()}, nil
}

// Event appends one line. A nil log is a no-op, so call sites need no checks.
func (l *SessionLog) Event(ev string, fields map[string]any) {
	if l == nil {
		return
	}
	rec := map[string]any{"t": time.Now().Format(time.RFC3339Nano), "ev": ev}
	for k, v := range fields {
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.Write(append(b, '\n'))
}

// screen is the UI sink: every Info/Warn/Error/Harness line, as the user saw it.
func (l *SessionLog) screen(kind, text string) {
	l.Event("screen", map[string]any{"kind": kind, "text": text})
}

func (l *SessionLog) Close() {
	if l != nil && l.f != nil {
		l.f.Close()
	}
}

// logClip keeps the log readable: long text is cut on a rune boundary.
func logClip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("… [%d more chars]", len(r)-n)
}

// logTool is the record of one tool result, whoever ran it: the model, the
// harness (verification gate), the preflight, or the final re-check.
func (a *Agent) logTool(res ToolResult, by string) {
	a.log.Event("tool", map[string]any{
		"by": by, "summary": res.Summary, "status": res.Status,
		"failed": res.Failed, "invalid": res.Invalid, "denied": res.Denied, "err_key": res.ErrKey,
		"mutated": res.Mutated, "verify": res.Verify, "output": logClip(res.Output, 4000),
	})
}
