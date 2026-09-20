package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func readEvents(t *testing.T, path string) []map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("baris log bukan JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestSessionLogRecordsATurn(t *testing.T) {
	a, _, _, _ := newFakeAgent(t,
		toolReply("bash", `{"command": "echo halo-dari-log"}`),
		textReply("Selesai."),
	)
	lg, err := OpenSessionLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.log, a.ui.sink = lg, lg.screen
	a.ui.Info("catatan layar")
	a.RunTurn(context.Background(), "coba")
	lg.Close()

	if st, _ := os.Stat(lg.Path); st.Mode().Perm() != 0o600 {
		t.Fatalf("log memuat output tool, izinnya harus 0600: %v", st.Mode().Perm())
	}
	evs := readEvents(t, lg.Path)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e["ev"].(string))
	}
	joined := strings.Join(kinds, ",")
	for _, want := range []string{"screen", "turn", "request", "reply", "tool", "request", "reply", "turn_end"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("event %q hilang dari %v", want, kinds)
		}
	}
	var tool, end map[string]any
	for _, e := range evs {
		switch e["ev"] {
		case "tool":
			tool = e
		case "turn_end":
			end = e
		}
	}
	if tool["by"] != "model" || !strings.Contains(tool["output"].(string), "halo-dari-log") || tool["summary"] != "bash echo halo-dari-log" {
		t.Fatalf("event tool: %v", tool)
	}
	if end["outcome"] != "answered" || end["steps"].(float64) != 1 {
		t.Fatalf("turn_end: %v", end)
	}
}

func TestSessionLogIsOptional(t *testing.T) {
	// A nil log must be safe everywhere: this is the --no-log path.
	a, _, _, _ := newFakeAgent(t, textReply("ok"))
	a.log = nil
	if err := a.RunTurn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	a.logTool(ToolResult{}, "model")
	a.log.Event("x", nil)
}

func TestLogClipIsRuneSafe(t *testing.T) {
	got := logClip("héllo wörld", 5)
	if !strings.HasPrefix(got, "héllo") || !strings.Contains(got, "6 more chars") {
		t.Fatalf("logClip: %q", got)
	}
	if logClip("pendek", 100) != "pendek" {
		t.Fatal("teks pendek tidak boleh diubah")
	}
}
