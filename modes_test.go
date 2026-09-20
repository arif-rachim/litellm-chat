package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAskUserTool(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		toolReply("ask_user", `{"question": "Pakai library form yang mana?", "options": ["react-hook-form", "formik", "tanpa library"]}`),
		textReply("Oke, pakai formik."),
	)
	var asked string
	a.askLine = func(_ context.Context, p string) (string, error) { asked = p; return "2", nil }
	a.RunTurn(context.Background(), "buat form")
	if !strings.Contains(asked, "jawab") {
		t.Fatalf("free-text prompt: %q", asked)
	}
	if tm := toolMsgs(a); len(tm) != 1 || !strings.Contains(tm[0].Content, "The user answered: formik") {
		t.Fatalf("a number should pick that option: %+v", tm)
	}
	if !strings.Contains(screen.String(), "1) react-hook-form") {
		t.Fatalf("options not shown:\n%s", screen)
	}
	if len(f.reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(f.reqs))
	}

	// Free text is passed through as-is.
	a, _, _, _ = newFakeAgent(t, toolReply("ask_user", `{"question": "Nama filenya?"}`), textReply("ok"))
	a.askLine = func(context.Context, string) (string, error) { return "src/form.tsx", nil }
	a.RunTurn(context.Background(), "go")
	if tm := toolMsgs(a); !strings.Contains(tm[0].Content, "src/form.tsx") {
		t.Fatalf("free text: %q", tm[0].Content)
	}

	// Nobody to ask: the model is told to decide instead of hanging.
	a, _, _, _ = newFakeAgent(t, toolReply("ask_user", `{"question": "x?"}`), textReply("ok"))
	a.RunTurn(context.Background(), "go")
	if tm := toolMsgs(a); !strings.Contains(tm[0].Content, "Nobody can answer") {
		t.Fatalf("non-interactive: %q", tm[0].Content)
	}
}

func TestPlanMode(t *testing.T) {
	a, f, dir, _ := newFakeAgent(t,
		toolReply("write_file", `{"path": "x.js", "content": "x"}`),
		textReply("Rencana: ubah x.js, lalu jalankan npm test."),
		toolReply("write_file", `{"path": "x.js", "content": "ok"}`),
		textReply("Selesai."),
	)
	a.cfg.Mode = "plan"
	a.ask = func(context.Context, string) (string, error) { return "y", nil }
	a.RunTurn(context.Background(), "ubah x.js")

	tm := toolMsgs(a)
	if !strings.Contains(tm[0].Content, "Plan mode is on") {
		t.Fatalf("write should be blocked in plan mode: %q", tm[0].Content)
	}
	if a.cfg.Mode != "auto" {
		t.Fatalf("approving the plan should switch to auto, got %q", a.cfg.Mode)
	}
	// The blocked write never landed; only the one after approval did.
	if b, err := os.ReadFile(filepath.Join(dir, "x.js")); err != nil || string(b) != "ok" {
		t.Fatalf("after approval the write should go through, and only then: %q %v", b, err)
	}
	if len(f.reqs) != 4 {
		t.Fatalf("requests = %d, want 4", len(f.reqs))
	}
	// The model is told about plan mode, and stops being told once it is off.
	sys := func(i int) string {
		b, _ := f.reqs[i]["messages"].([]any)
		m, _ := b[0].(map[string]any)
		return m["content"].(string)
	}
	if !strings.Contains(sys(0), "PLAN") || strings.Contains(sys(3), "PLAN") {
		t.Fatal("mode note not attached/removed correctly")
	}
}

func TestPlanModeDeclined(t *testing.T) {
	a, f, _, _ := newFakeAgent(t, textReply("Rencananya begini."))
	a.cfg.Mode = "plan"
	a.ask = func(context.Context, string) (string, error) { return "t", nil }
	a.RunTurn(context.Background(), "rencanakan")
	if a.cfg.Mode != "plan" || len(f.reqs) != 1 {
		t.Fatalf("declining should keep plan mode (mode=%s, requests=%d)", a.cfg.Mode, len(f.reqs))
	}
}

func TestAutoEditMode(t *testing.T) {
	a, _, dir, _ := newFakeAgent(t,
		toolReply("write_file", `{"path": "x.txt", "content": "hi"}`),
		toolReply("bash", `{"command": "echo hi"}`),
		textReply("Selesai."),
	)
	a.cfg.Yolo, a.cfg.Mode = false, "auto"
	var prompts []string
	a.ask = func(_ context.Context, p string) (string, error) { prompts = append(prompts, p); return "y", nil }
	a.RunTurn(context.Background(), "go")
	if b, err := os.ReadFile(filepath.Join(dir, "x.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("auto mode should write without asking: %v", err)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "bash") {
		t.Fatalf("bash must still be asked in auto mode: %v", prompts)
	}
	// Risky paths keep asking even in auto mode.
	a, _, _, _ = newFakeAgent(t, toolReply("write_file", `{"path": "../escape.txt", "content": "x"}`), textReply("ok"))
	a.cfg.Yolo, a.cfg.Mode = false, "auto"
	prompts = nil
	a.ask = func(_ context.Context, p string) (string, error) { prompts = append(prompts, p); return "n", nil }
	a.RunTurn(context.Background(), "go")
	if len(prompts) != 1 || !strings.Contains(prompts[0], "di luar folder proyek") {
		t.Fatalf("risky write in auto mode must be asked: %v", prompts)
	}
}

// testEditor drives the line editor from a byte string, with no real terminal.
func testEditor(keys string, onTab func()) (string, string, error) {
	return testEditorCmds(keys, onTab, nil)
}

func testEditorCmds(keys string, onTab func(), cmds []Completion) (string, string, error) {
	var out bytes.Buffer
	e := &Editor{in: bufio.NewReader(strings.NewReader(keys)), out: &out, onTab: onTab, cmds: cmds}
	line, err := e.ReadLine(func() string { return "› " })
	return line, out.String(), err
}

func TestLineEditor(t *testing.T) {
	if line, _, err := testEditor("halo dunia\r", nil); line != "halo dunia" || err != nil {
		t.Fatalf("plain typing: %q %v", line, err)
	}
	// backspace, then left arrow + insert
	if line, _, _ := testEditor("abcx\x7f\x1b[D\x1b[DZ\r", nil); line != "aZbc" {
		t.Fatalf("editing: %q", line)
	}
	if line, _, _ := testEditor("satu dua\x17tiga\r", nil); line != "satu tiga" {
		t.Fatalf("ctrl-w: %q", line)
	}
	if line, _, _ := testEditor("buang semua\x15baru\r", nil); line != "baru" {
		t.Fatalf("ctrl-u: %q", line)
	}
	if _, _, err := testEditor("\x03", nil); err != errInterrupt {
		t.Fatalf("ctrl-c should interrupt, got %v", err)
	}
	if _, _, err := testEditor("\x04", nil); err != io.EOF {
		t.Fatalf("ctrl-d on an empty line should be EOF, got %v", err)
	}
	// Tab does not reach the line; it switches the mode instead.
	mode := "ask"
	line, _, _ := testEditor("ab\tcd\r", func() { mode = "auto" })
	if line != "abcd" || mode != "auto" {
		t.Fatalf("tab: line=%q mode=%q", line, mode)
	}
}

func TestEditorHistory(t *testing.T) {
	var out bytes.Buffer
	e := &Editor{in: bufio.NewReader(strings.NewReader("pertama\rkedua\r\x1b[A\x1b[A\r")), out: &out}
	p := func() string { return "› " }
	e.ReadLine(p)
	e.ReadLine(p)
	line, _ := e.ReadLine(p)
	if line != "pertama" {
		t.Fatalf("two ups should recall the first line, got %q", line)
	}
}

func TestReadChoiceSingleKey(t *testing.T) {
	var out bytes.Buffer
	e := &Editor{in: bufio.NewReader(strings.NewReader("qY")), out: &out}
	ans, err := e.ReadChoice("izinkan? [y/N/a] ", "yna")
	if ans != "y" || err != nil {
		t.Fatalf("unknown keys are ignored, Y accepted: %q %v", ans, err)
	}
	e = &Editor{in: bufio.NewReader(strings.NewReader("\r")), out: &out}
	if ans, _ := e.ReadChoice("izinkan? ", "yna"); ans != "" {
		t.Fatalf("enter means the default, got %q", ans)
	}
}

func TestSlashCompletion(t *testing.T) {
	cmds := []Completion{
		{"/clear", "", "reset"},
		{"/config", "", "setel"},
		{"/model", "[filter]", "ganti model"},
	}
	// One candidate: Tab finishes the name, and an argument gets a space.
	if line, _, _ := testEditorCmds("/mo\t\r", nil, cmds); line != "/model " {
		t.Fatalf("single match: %q", line)
	}
	// Several candidates: Tab stops at what they share, mode is not switched.
	mode := "ask"
	line, out, _ := testEditorCmds("/c\t\r", func() { mode = "auto" }, cmds)
	if line != "/c" || mode != "ask" {
		t.Fatalf("common prefix: line=%q mode=%q", line, mode)
	}
	if !strings.Contains(out, "/clear") || !strings.Contains(out, "/config") {
		t.Fatalf("candidates should be listed while typing: %q", out)
	}
	// The list only offers what the typed prefix can still become.
	e := &Editor{cmds: cmds}
	if rows := e.menu("/c"); len(rows) != 2 || !strings.Contains(rows[0], "/clear") {
		t.Fatalf("suggestions for /c: %q", rows)
	}
	if rows := e.menu("/model q"); rows != nil {
		t.Fatalf("an argument is not a command word: %q", rows)
	}
	// Past the command word Tab is the mode switch again.
	mode = "ask"
	if line, _, _ := testEditorCmds("/model q\t\r", func() { mode = "auto" }, cmds); line != "/model q" || mode != "auto" {
		t.Fatalf("tab in an argument: line=%q mode=%q", line, mode)
	}
	// The suggestion rows are wiped before the line is handed over.
	if _, out, _ := testEditorCmds("/c\r", nil, cmds); !strings.Contains(out, "\x1b[2A\n") {
		t.Fatalf("menu not cleared on enter: %q", out)
	}
}
