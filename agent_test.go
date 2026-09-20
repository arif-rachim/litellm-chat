package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeLLM replays scripted SSE replies and records every request body.
type fakeLLM struct {
	mu      sync.Mutex
	replies []string
	reqs    []map[string]any
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	i := len(f.reqs)
	f.reqs = append(f.reqs, body)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	if i >= len(f.replies) {
		io.WriteString(w, textReply("(no more scripted replies)"))
		return
	}
	io.WriteString(w, f.replies[i])
}

func sse(chunks ...map[string]any) string {
	var b strings.Builder
	for _, c := range chunks {
		j, _ := json.Marshal(c)
		b.WriteString("data: " + string(j) + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func delta(d map[string]any) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"delta": d}}}
}

func textReply(s string) string { return sse(delta(map[string]any{"content": s})) }

func toolReply(name, args string) string {
	return sse(delta(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "c1", "function": map[string]any{"name": name, "arguments": args}}}}))
}

func newFakeAgent(t *testing.T, replies ...string) (*Agent, *fakeLLM, string, *bytes.Buffer) {
	f := &fakeLLM{replies: replies}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644)
	cfg := &Config{BaseURL: srv.URL, Model: "qwen3.5-35b-a3b", ToolMax: 8000, MaxSteps: 12, Think: "auto", Yolo: true}
	var screen bytes.Buffer
	ui := NewUI(&screen, &screen, false, false, false)
	return NewAgent(cfg, ui, nil, nil, nil, dir), f, dir, &screen
}

func toolMsgs(a *Agent) []Message {
	var out []Message
	for _, m := range a.msgs {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

func thinkFlags(f *fakeLLM) []bool {
	var out []bool
	for _, r := range f.reqs {
		kw, _ := r["chat_template_kwargs"].(map[string]any)
		v, _ := kw["enable_thinking"].(bool)
		out = append(out, v)
	}
	return out
}

func TestStreamedToolCallAndAdaptiveThinking(t *testing.T) {
	a, f, _, _ := newFakeAgent(t,
		// Reasoning plus a tool call whose arguments arrive in pieces.
		sse(
			delta(map[string]any{"reasoning_content": "I should read the file."}),
			delta(map[string]any{"content": "Reading a.txt."}),
			delta(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "function": map[string]any{"name": "read_file", "arguments": `{"pa`}}}}),
			delta(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `th": "a.txt"}`}}}}),
		),
		toolReply("bash", `{"command": "exit 3"}`),
		textReply("Done."),
	)
	if err := a.RunTurn(context.Background(), "read it"); err != nil {
		t.Fatal(err)
	}
	tm := toolMsgs(a)
	if len(tm) != 2 || !strings.Contains(tm[0].Content, "hello") {
		t.Fatalf("tool results: %+v", tm)
	}
	if !strings.Contains(tm[1].Content, "exit_code: 3") || !strings.Contains(tm[1].Content, "state the cause") {
		t.Fatalf("failed command should get a reflection hint: %q", tm[1].Content)
	}
	// Think to plan, not on a routine step, again after the failure.
	if got := thinkFlags(f); len(got) != 3 || !got[0] || got[1] || !got[2] {
		t.Fatalf("thinking per request = %v, want [true false true]", got)
	}
	// Reasoning must not be sent back in the history.
	for _, r := range f.reqs {
		b, _ := json.Marshal(r["messages"])
		if strings.Contains(string(b), "I should read the file") {
			t.Fatal("reasoning leaked into history")
		}
	}
	// Sampling follows the thinking decision.
	if f.reqs[0]["temperature"] != 0.6 || f.reqs[1]["temperature"] != 0.7 {
		t.Fatalf("sampling: %v / %v", f.reqs[0]["temperature"], f.reqs[1]["temperature"])
	}
}

func TestInvalidJSONStopsAfterThreeTries(t *testing.T) {
	bad := toolReply("read_file", `{"path": `)
	a, f, _, screen := newFakeAgent(t, bad, bad, bad, textReply("never reached"))
	a.RunTurn(context.Background(), "go")
	if len(f.reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(f.reqs))
	}
	tm := toolMsgs(a)
	if !strings.Contains(tm[0].Content, "not valid JSON") || !strings.Contains(tm[0].Content, "Example:") {
		t.Fatalf("correction: %q", tm[0].Content)
	}
	if !strings.Contains(screen.String(), "3x") {
		t.Fatalf("user not told why the turn stopped:\n%s", screen)
	}
}

func TestUnknownToolAndTextToolCall(t *testing.T) {
	a, _, _, _ := newFakeAgent(t,
		toolReply("grep_search", `{"q": "x"}`),
		textReply("Let me read.\n<tool_call>\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.txt\"}}\n</tool_call>"),
		textReply("Done."),
	)
	a.RunTurn(context.Background(), "go")
	tm := toolMsgs(a)
	if len(tm) != 2 || !strings.Contains(tm[0].Content, "There is no tool") || !strings.Contains(tm[0].Content, "bash(") {
		t.Fatalf("unknown tool correction: %+v", tm)
	}
	if !strings.Contains(tm[1].Content, "hello") {
		t.Fatalf("text tool call not executed: %q", tm[1].Content)
	}
	for _, m := range a.msgs {
		if m.Role == "assistant" && strings.Contains(m.Content, "<tool_call>") {
			t.Fatal("tool call text left in assistant content")
		}
	}
}

func TestLoopDetection(t *testing.T) {
	r := toolReply("read_file", `{"path": "a.txt"}`)
	a, _, _, _ := newFakeAgent(t, r, r, r, textReply("Done."))
	a.RunTurn(context.Background(), "go")
	tm := toolMsgs(a)
	if len(tm) != 3 || !strings.Contains(tm[2].Content, "same arguments 3 times") {
		t.Fatalf("third identical call should be refused: %q", tm[len(tm)-1].Content)
	}
}

func TestEmptyReplyNudge(t *testing.T) {
	a, f, _, _ := newFakeAgent(t, textReply(""), textReply("Answer."))
	a.RunTurn(context.Background(), "go")
	if len(f.reqs) != 2 || !strings.Contains(a.msgs[len(a.msgs)-2].Content, "reply was empty") {
		t.Fatalf("expected one nudge, got %d requests", len(f.reqs))
	}
}

func TestVerifyReminder(t *testing.T) {
	a, f, _, _ := newFakeAgent(t,
		toolReply("write_file", `{"path": "config.yaml", "content": "a: 1"}`),
		textReply("Selesai."),
		textReply("Tidak perlu diverifikasi: hanya config."),
	)
	a.RunTurn(context.Background(), "tulis config")
	if len(f.reqs) != 3 || !strings.Contains(a.msgs[len(a.msgs)-2].Content, "did not verify") {
		t.Fatalf("expected a verification reminder (requests=%d)", len(f.reqs))
	}

	// Prose files need no verification.
	a, f, _, _ = newFakeAgent(t, toolReply("write_file", `{"path": "notes.md", "content": "hi"}`), textReply("Selesai."))
	a.RunTurn(context.Background(), "tulis notes")
	if len(f.reqs) != 2 {
		t.Fatalf("no reminder expected for a .md file (requests=%d)", len(f.reqs))
	}

	// A single JS file whose auto-check passed counts as verified.
	if !hasNode() {
		t.Skip("node not installed")
	}
	a, f, _, _ = newFakeAgent(t,
		toolReply("write_file", `{"path": "x.js", "content": "export const x = 1;\n"}`),
		textReply("Selesai."),
	)
	a.RunTurn(context.Background(), "tulis x.js")
	if len(f.reqs) != 2 {
		t.Fatalf("no reminder expected after a passing auto-check (requests=%d)", len(f.reqs))
	}
}

func TestTodoReminderAndPlanLine(t *testing.T) {
	a, f, _, _ := newFakeAgent(t,
		toolReply("todo", `{"items": [{"text": "read", "status": "done"}, {"text": "fix", "status": "pending"}]}`),
		textReply("Done."),
		textReply("Step fix was not needed."),
	)
	a.RunTurn(context.Background(), "go")
	if tm := toolMsgs(a); !strings.Contains(tm[0].Content, "[plan] [x] read | [ ] fix") {
		t.Fatalf("plan line missing: %q", tm[0].Content)
	}
	if len(f.reqs) != 3 || !strings.Contains(a.msgs[len(a.msgs)-2].Content, "unfinished steps: fix") {
		t.Fatalf("expected a todo reminder (requests=%d)", len(f.reqs))
	}
}

func TestContextBudget(t *testing.T) {
	a, _, _, _ := newFakeAgent(t)
	a.cfg.Ctx = 4000
	big := strings.Repeat("x", 3000)
	a.msgs = append(a.msgs, Message{Role: "user", Content: "first"})
	for i := 0; i < 6; i++ {
		a.msgs = append(a.msgs,
			Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c", Function: FunctionCall{Name: "bash"}}}},
			Message{Role: "tool", ToolCallID: "c", Content: big, summary: "bash ls"})
	}
	a.turnStart = len(a.msgs)
	a.msgs = append(a.msgs, Message{Role: "user", Content: "now"})
	a.fitContext()
	if a.msgs[1].Content != "first" || a.msgs[len(a.msgs)-1].Content != "now" {
		t.Fatal("system prompt, first and current user message must stay")
	}
	if estTokens(a.msgs) >= 4000*90/100 {
		t.Fatalf("still over budget: %d tokens", estTokens(a.msgs))
	}
	for i, m := range a.msgs {
		if m.Role == "tool" && (i == 0 || (a.msgs[i-1].Role != "assistant" && a.msgs[i-1].Role != "tool")) {
			t.Fatal("orphan tool message after dropping")
		}
	}
}

func TestProbe(t *testing.T) {
	// kind: kwargs (thinking switchable with chat_template_kwargs), always, none (tool calls as hermes text)
	serve := func(kind string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "text/event-stream")
			think := false
			switch kind {
			case "kwargs":
				kw, _ := body["chat_template_kwargs"].(map[string]any)
				think = kw["enable_thinking"] != false
			case "always":
				think = true
			}
			var chunks []map[string]any
			if think {
				chunks = append(chunks, delta(map[string]any{"reasoning_content": "hmm"}))
			}
			if body["tools"] != nil {
				if kind == "none" {
					chunks = append(chunks, delta(map[string]any{"content": `<tool_call>{"name": "get_weather", "arguments": {"city": "Jakarta"}}</tool_call>`}))
				} else {
					chunks = append(chunks, delta(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "1", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"Jakarta"}`}}}}))
				}
			} else {
				chunks = append(chunks, delta(map[string]any{"content": "42"}))
			}
			io.WriteString(w, sse(chunks...))
		}))
	}
	for kind, want := range map[string]string{"kwargs": "chat_template_kwargs", "always": "always", "none": "none"} {
		srv := serve(kind)
		base := SelectProfile("m", srv.URL, nil)
		p := RunProbe(context.Background(), NewClient(srv.URL, ""), "m", base, io.Discard)
		srv.Close()
		if p.Thinking.Control != want {
			t.Errorf("%s: control = %s, want %s", kind, p.Thinking.Control, want)
		}
		if kind == "none" && p.ToolFormat[1] != "hermes" {
			t.Errorf("none: tool_format = %v, want hermes first after native", p.ToolFormat)
		}
	}
	// Saving replaces a profile with the same match.
	path := filepath.Join(t.TempDir(), "m.json")
	p := &Profile{Match: "m", Thinking: Thinking{Control: "none"}}
	SaveProfile(path, p)
	p.Thinking.Control = "always"
	SaveProfile(path, p)
	user, err := LoadUserProfiles(path)
	if err != nil || len(user) != 1 || user[0].Thinking.Control != "always" {
		t.Fatalf("saved profiles: %+v %v", user, err)
	}
}

func TestDenialStopsTurn(t *testing.T) {
	a, f, dir, _ := newFakeAgent(t,
		toolReply("write_file", `{"path": "hello.txt", "content": "halo"}`),
		toolReply("bash", `{"command": "echo halo > hello.txt"}`),
	)
	a.cfg.Yolo = false
	a.ask = func(context.Context, string) (string, error) { return "n", nil }
	a.RunTurn(context.Background(), "buat hello.txt")
	if len(f.reqs) != 1 {
		t.Fatalf("model got %d requests after a denial, want 1", len(f.reqs))
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err == nil {
		t.Fatal("file written despite denial")
	}
	if tm := toolMsgs(a); len(tm) != 1 || !strings.Contains(tm[0].Content, "denied") {
		t.Fatalf("denial not recorded: %+v", tm)
	}
}
