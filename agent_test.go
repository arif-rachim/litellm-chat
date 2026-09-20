package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// --- Pencarian tanpa loop: catatan percobaan, tanda kegagalan, tangga eskalasi

func TestFailSignature(t *testing.T) {
	a := failSignature("exit_code: 1\nTypeError: x is undefined\n    at /home/me/app/src/a.js:12:3")
	b := failSignature("exit_code: 2\nTypeError: x is undefined\n    at /srv/other/b.js:481:9")
	if a != b {
		t.Fatalf("kegagalan yang sama di tempat berbeda harus punya tanda sama:\n a=%q\n b=%q", a, b)
	}
	if !strings.Contains(a, "typeerror: x is undefined") {
		t.Fatalf("tanda kehilangan inti pesannya: %q", a)
	}
	if strings.Contains(a, "12") || strings.Contains(a, "/home/me") {
		t.Fatalf("nomor baris / path harus dinormalkan: %q", a)
	}
	if c := failSignature("exit_code: 1\nsyntax error near unexpected token"); c == a {
		t.Fatalf("kegagalan berbeda tidak boleh punya tanda sama: %q", c)
	}
	// A status-only line carries no signature; noteResult falls back per tool,
	// so two silent failures from different tools are not "the same wall".
	if s := failSignature("exit_code: 1\n"); s != "" {
		t.Fatalf("tidak ada pesan error, tanda harus kosong: %q", s)
	}
	st := &turnState{}
	st.noteResult("bash", ToolResult{Failed: true, Output: "exit_code: 1\n", Summary: "bash x"})
	st.noteResult("edit_file", ToolResult{Failed: true, Output: "", Summary: "edit_file a.go"})
	if st.failStreak != 1 {
		t.Fatalf("kegagalan sunyi dari tool berbeda bukan tembok yang sama: %d", st.failStreak)
	}
}

func failRes(out string) ToolResult {
	return ToolResult{Failed: true, Output: out, Summary: "bash npm test"}
}

func TestEscalationLadder(t *testing.T) {
	const errOut = "exit_code: 1\nError: cannot find module 'auth'"
	st := &turnState{}
	step := func(canAsk bool) (string, bool) {
		st.noteResult("bash", failRes(errOut))
		_, msg, stop := st.escalate(canAsk)
		return msg, stop
	}
	if msg, _ := step(true); msg != "" {
		t.Fatalf("kegagalan pertama belum boleh eskalasi: %q", msg)
	}
	msg, stop := step(true)
	if stop || !strings.Contains(msg, "proven wrong") {
		t.Fatalf("anak tangga 2 harus meminta diagnosis: %q", msg)
	}
	msg, stop = step(true)
	if stop || !strings.Contains(msg, "Change the kind of step") {
		t.Fatalf("anak tangga 3 harus memaksa ganti jenis langkah: %q", msg)
	}
	msg, stop = step(true)
	if stop || !strings.Contains(msg, "ask_user") {
		t.Fatalf("anak tangga 4 harus menyuruh bertanya: %q", msg)
	}
	if msg, stop = step(true); !stop || msg == "" {
		t.Fatalf("anak tangga terakhir harus menghentikan giliran: %q stop=%v", msg, stop)
	}
	// Tanpa cara bertanya (mode -p), tangga ask_user dilewati: langsung berhenti.
	st = &turnState{}
	for i := 0; i < 3; i++ {
		st.noteResult("bash", failRes(errOut))
		st.escalate(false)
	}
	st.noteResult("bash", failRes(errOut))
	if _, _, stop := st.escalate(false); !stop {
		t.Fatal("tanpa ask_user, giliran harus berhenti di anak tangga 4")
	}
}

func TestLedgerRemembersAttempts(t *testing.T) {
	st := &turnState{}
	st.noteResult("bash", failRes("exit_code: 1\nError: cannot find module 'auth'"))
	st.noteResult("read_file", ToolResult{Summary: "read_file src/auth.js", Status: "baris 1-20 dari 20"})
	// Perintah lain, kegagalan sama: harus dikenali sebagai tembok yang sama.
	st.noteResult("bash", ToolResult{Failed: true, Summary: "bash node src/app.js",
		Output: "exit_code: 1\nError: cannot find module 'auth'"})
	led := st.ledger()
	for _, want := range []string{"already attempted", "bash npm test", "read_file src/auth.js", "same failure as #1", "survived 2 attempts"} {
		if !strings.Contains(led, want) {
			t.Fatalf("catatan percobaan kehilangan %q:\n%s", want, led)
		}
	}
	if st.failStreak != 2 {
		t.Fatalf("dua perintah berbeda dengan error sama = rentetan 2, dapat %d", st.failStreak)
	}
	// Verifikasi yang lulus adalah kemajuan nyata: pencarian dimulai lagi.
	st.noteResult("bash", ToolResult{Verify: true, Summary: "bash npm test", Status: "exit 0"})
	if st.failStreak != 0 || st.failSig != "" {
		t.Fatalf("verifikasi lulus harus mengosongkan rentetan: %d %q", st.failStreak, st.failSig)
	}
	// Catatan lama dibuang, tidak tumbuh tanpa batas.
	for i := 0; i < maxLedger+5; i++ {
		st.noteResult("bash", ToolResult{Summary: "bash echo hi"})
	}
	if len(st.attempts) != maxLedger {
		t.Fatalf("catatan harus dibatasi %d, dapat %d", maxLedger, len(st.attempts))
	}
}

func TestLedgerTravelsInEveryRequest(t *testing.T) {
	fail := toolReply("bash", `{"command": "cat tidak-ada.txt"}`)
	a, f, _, _ := newFakeAgent(t, fail, fail, textReply("selesai"))
	a.RunTurn(context.Background(), "perbaiki")
	if len(f.reqs) < 2 {
		t.Fatalf("permintaan = %d", len(f.reqs))
	}
	sys := func(i int) string {
		msgs, _ := f.reqs[i]["messages"].([]any)
		m, _ := msgs[0].(map[string]any)
		s, _ := m["content"].(string)
		return s
	}
	if strings.Contains(sys(0), "already attempted") {
		t.Fatal("permintaan pertama belum punya percobaan apa pun")
	}
	if !strings.Contains(sys(1), "already attempted") || !strings.Contains(sys(1), "cat tidak-ada.txt") {
		t.Fatalf("catatan percobaan tidak ikut dikirim:\n%s", sys(1))
	}
	// Catatan hidup di pesan sistem, yang dibangun ulang tiap permintaan, jadi
	// pemangkasan konteks tidak bisa menghapusnya.
	if strings.Contains(sys(1), "[old output removed") {
		t.Fatal("catatan percobaan tidak boleh ikut diringkas")
	}
}

// TestLadderChangesShapeOfTask memeriksa urutan lengkapnya seperti yang dilihat
// model: koreksi yang berbeda-beda, bukan omelan yang sama berulang.
func TestLadderChangesShapeOfTask(t *testing.T) {
	fail := toolReply("bash", `{"command": "cat tidak-ada.txt"}`)
	fail2 := toolReply("bash", `{"command": "cat ./tidak-ada.txt"}`)
	a, _, _, screen := newFakeAgent(t, fail, fail2, fail, fail2, fail, textReply("menyerah"))
	a.askLine = func(ctx context.Context, p string) (string, error) { return "", nil } // bisa bertanya
	a.RunTurn(context.Background(), "buka file itu")

	var harness []string
	for _, m := range a.msgs {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[harness]") {
			harness = append(harness, m.Content)
		}
	}
	if len(harness) < 3 {
		t.Fatalf("harness hanya mengirim %d koreksi:\n%s", len(harness), strings.Join(harness, "\n--\n"))
	}
	joined := strings.Join(harness, "\n")
	for _, want := range []string{"proven wrong", "Change the kind of step", "ask_user"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("tangga eskalasi kehilangan %q:\n%s", want, joined)
		}
	}
	// Tiap koreksi harus berbeda: pengulangan kalimat yang sama justru yang
	// membuat model kecil mengabaikannya.
	for i := 1; i < len(harness); i++ {
		if harness[i] == harness[i-1] {
			t.Fatalf("koreksi ke-%d sama persis dengan sebelumnya:\n%s", i, harness[i])
		}
	}
	if !strings.Contains(screen.String(), "kegagalan yang sama") {
		t.Fatalf("user tidak diberi tahu ada rentetan kegagalan:\n%s", screen)
	}
}

// --- Tahap 1: gerbang verifikasi

func TestIsVerifyCmd(t *testing.T) {
	project := []string{"pnpm run typecheck", "go build ./...", "go test ./..."}
	cases := []struct {
		cmd  string
		want bool
	}{
		{"go test ./...", true}, {"go test ./pkg -run TestX", true}, {"go build ./...", true},
		{"npm test", true}, {"npm test -- math", true}, {"pnpm run typecheck", true},
		{"python3 -m pytest", true}, {"pytest tests/", true}, {"cargo test", true},
		{"make test", true}, {"test -f out.txt", true}, {"[ -f out.txt ]", true},
		{"grep -q Usage README.md", true}, {"git diff --exit-code", true}, {"diff a b", true},
		{"cd sub && go test ./...", true}, {"CI=1 npm test", true}, {"ls; go vet ./...", true},
		// Running a program proves nothing about it.
		{"node index.js", false}, {"python3 script.py", false}, {"npx something", false},
		{"make", false}, {"cat file", false}, {"ls -la", false}, {"echo ok", false},
		{"node -e \"require('./a')\"", false}, {"go run .", false}, {"npm start", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isVerifyCmd(c.cmd, project); got != c.want {
			t.Errorf("isVerifyCmd(%q) = %v, mau %v", c.cmd, got, c.want)
		}
	}
}

func TestHarnessRunsVerifier(t *testing.T) {
	// Model bilang selesai tanpa memverifikasi: harness menjalankan perintah
	// verifikasi proyek sendiri, lulus, jawaban diterima tanpa omelan.
	a, f, _, screen := newFakeAgent(t,
		toolReply("write_file", `{"path": "config.yaml", "content": "a: 1"}`),
		textReply("Selesai."),
	)
	a.env.VerifyCmds = []string{"test -f config.yaml"}
	a.RunTurn(context.Background(), "tulis config")
	if len(f.reqs) != 2 {
		t.Fatalf("verifikasi lulus harus menerima jawaban tanpa permintaan tambahan: %d", len(f.reqs))
	}
	if !strings.Contains(screen.String(), "harness menjalankan test -f config.yaml") || !strings.Contains(screen.String(), "verifikasi lulus") {
		t.Fatalf("user tidak melihat harness memverifikasi:\n%s", screen)
	}
	for _, m := range a.msgs {
		if strings.Contains(m.Content, "did not verify") {
			t.Fatal("pengingat lama tidak boleh muncul kalau harness bisa memverifikasi sendiri")
		}
	}

	// Gagal: hasilnya kembali ke model sebagai bukti, dan kalau terus gagal,
	// tangga eskalasi yang sama menghentikan giliran.
	a, f, _, screen = newFakeAgent(t,
		toolReply("write_file", `{"path": "config.yaml", "content": "a: 1"}`),
		textReply("Selesai."),
		textReply("Sudah saya perbaiki."),
	)
	a.env.VerifyCmds = []string{"test -f tidak-ada.txt"}
	a.RunTurn(context.Background(), "tulis config")
	var evidence, ladder int
	for _, m := range a.msgs {
		if m.Role == "user" && strings.Contains(m.Content, "exited non-zero") && strings.Contains(m.Content, "exit_code: 1") {
			evidence++
		}
		if strings.Contains(m.Content, "proven wrong") {
			ladder++
		}
	}
	if evidence == 0 || ladder == 0 {
		t.Fatalf("kegagalan verifikasi harus jadi bukti (%d) dan menaiki tangga (%d)", evidence, ladder)
	}
	if !strings.Contains(screen.String(), "giliran dihentikan") {
		t.Fatalf("verifikasi yang terus gagal harus menghentikan giliran:\n%s", screen)
	}
	if len(f.reqs) > 6 {
		t.Fatalf("terlalu banyak putaran sebelum berhenti: %d", len(f.reqs))
	}

	// Tanpa perintah verifikasi yang dikenal, jalur lama (pengingat) tetap ada.
	a, f, _, _ = newFakeAgent(t,
		toolReply("write_file", `{"path": "config.yaml", "content": "a: 1"}`),
		textReply("Selesai."),
		textReply("Tidak perlu."),
	)
	a.RunTurn(context.Background(), "tulis config")
	if len(f.reqs) != 3 || !strings.Contains(a.msgs[len(a.msgs)-2].Content, "did not verify") {
		t.Fatalf("pengingat harus tetap ada tanpa VerifyCmds (requests=%d)", len(f.reqs))
	}
}

func TestTodoCheckmarksAreHarnessOwned(t *testing.T) {
	a, _, _, screen := newFakeAgent(t,
		toolReply("todo", `{"items": [{"text": "fix config", "status": "in_progress"}]}`),
		toolReply("write_file", `{"path": "config.yaml", "content": "a: 1"}`),
		toolReply("todo", `{"items": [{"text": "fix config", "status": "done"}]}`), // ditolak
		toolReply("bash", `{"command": "test -f config.yaml"}`),
		toolReply("todo", `{"items": [{"text": "fix config", "status": "done"}]}`), // diterima
		textReply("Selesai."),
	)
	a.RunTurn(context.Background(), "perbaiki config")
	tm := toolMsgs(a)
	if len(tm) < 5 {
		t.Fatalf("panggilan tool = %d", len(tm))
	}
	if !strings.Contains(tm[2].Content, "[>] fix config") || !strings.Contains(tm[2].Content, "not accepted as done") {
		t.Fatalf("centang tanpa verifikasi harus diturunkan:\n%s", tm[2].Content)
	}
	if !strings.Contains(tm[4].Content, "[x] fix config") || strings.Contains(tm[4].Content, "not accepted") {
		t.Fatalf("centang setelah verifikasi harus diterima:\n%s", tm[4].Content)
	}
	if len(a.todos) != 1 || a.todos[0].Status != "done" {
		t.Fatalf("status akhir: %+v", a.todos)
	}
	if !strings.Contains(screen.String(), "centang todo ditolak") {
		t.Fatalf("user tidak diberi tahu:\n%s", screen)
	}

	// Langkah membaca (tidak ada mutasi) boleh langsung dicentang; daftar yang
	// menyusut diumumkan.
	a, _, _, screen = newFakeAgent(t,
		toolReply("todo", `{"items": ["baca a.txt", "ubah a.txt", "uji"]}`),
		toolReply("read_file", `{"path": "a.txt"}`),
		toolReply("todo", `{"items": [{"text": "baca a.txt", "status": "done"}]}`),
		textReply("Selesai."),
	)
	a.RunTurn(context.Background(), "go")
	if len(a.todos) != 1 || a.todos[0].Status != "done" {
		t.Fatalf("langkah baca tanpa mutasi harus boleh dicentang: %+v", a.todos)
	}
	if !strings.Contains(screen.String(), "rencana diubah: 2 langkah dihapus") {
		t.Fatalf("daftar yang menyusut harus diumumkan:\n%s", screen)
	}
}

// --- Tahap 2: sinyal kemajuan

func TestSecondEditNeedsRead(t *testing.T) {
	a, _, dir, _ := newFakeAgent(t,
		toolReply("edit_file", `{"path": "a.txt", "old_string": "hello", "new_string": "halo"}`),
		toolReply("edit_file", `{"path": "a.txt", "old_string": "halo", "new_string": "hola"}`), // ditolak: belum dibaca ulang
		toolReply("read_file", `{"path": "a.txt"}`),
		toolReply("edit_file", `{"path": "a.txt", "old_string": "halo", "new_string": "hola"}`), // sekarang boleh
		textReply("Selesai."),
	)
	a.RunTurn(context.Background(), "ubah sapaan")
	tm := toolMsgs(a)
	if len(tm) < 4 {
		t.Fatalf("panggilan tool = %d", len(tm))
	}
	if !strings.Contains(tm[1].Content, "have not read it since") {
		t.Fatalf("edit kedua tanpa baca ulang harus ditolak:\n%s", tm[1].Content)
	}
	if !strings.Contains(tm[3].Content, "Edited a.txt") {
		t.Fatalf("edit setelah baca ulang harus diterima:\n%s", tm[3].Content)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "hola\n" {
		t.Fatalf("isi akhir: %q", b)
	}
}

func TestOscillationIsCaught(t *testing.T) {
	a, _, _, screen := newFakeAgent(t,
		toolReply("edit_file", `{"path": "a.txt", "old_string": "hello", "new_string": "halo"}`),
		toolReply("read_file", `{"path": "a.txt"}`),
		toolReply("edit_file", `{"path": "a.txt", "old_string": "halo", "new_string": "hello"}`), // kembali ke awal
		textReply("Sudah."),
	)
	a.RunTurn(context.Background(), "coba-coba")
	tm := toolMsgs(a)
	if len(tm) < 3 || !strings.Contains(tm[2].Content, "already had earlier this turn") {
		t.Fatalf("kembali ke isi semula harus dikenali:\n%s", tm[len(tm)-1].Content)
	}
	var diagnosis bool
	for _, m := range a.msgs {
		if m.Role == "user" && strings.Contains(m.Content, "proven wrong") {
			diagnosis = true
		}
	}
	if !diagnosis {
		t.Fatal("osilasi harus langsung ke anak tangga diagnosis")
	}
	if !strings.Contains(screen.String(), "osilasi") {
		t.Fatalf("user tidak diberi tahu:\n%s", screen)
	}
}

func TestNoProgressBudget(t *testing.T) {
	echo := func(n int) string { return toolReply("bash", fmt.Sprintf(`{"command": "echo %d"}`, n)) }
	var replies []string
	for i := 1; i <= 8; i++ {
		replies = append(replies, echo(i))
	}
	replies = append(replies, textReply("Selesai."))
	a, _, _, screen := newFakeAgent(t, replies...)
	a.RunTurn(context.Background(), "cari")
	warned := 0
	for _, m := range a.msgs {
		if m.Role == "user" && strings.Contains(m.Content, "without new evidence") {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("delapan langkah tanpa bukti harus memicu tepat satu peringatan, dapat %d", warned)
	}
	if !strings.Contains(screen.String(), "tanpa bukti baru") {
		t.Fatalf("user tidak diberi tahu:\n%s", screen)
	}

	// Satu bukti (file yang belum dibaca) di tengah memulai jendela lagi.
	replies = nil
	for i := 1; i <= 4; i++ {
		replies = append(replies, echo(i))
	}
	replies = append(replies, toolReply("read_file", `{"path": "a.txt"}`))
	for i := 5; i <= 9; i++ {
		replies = append(replies, echo(i))
	}
	replies = append(replies, textReply("Selesai."))
	a, _, _, _ = newFakeAgent(t, replies...)
	a.RunTurn(context.Background(), "cari")
	for _, m := range a.msgs {
		if strings.Contains(m.Content, "without new evidence") {
			t.Fatal("bukti baru harus memulai jendela lagi; tidak boleh ada peringatan")
		}
	}
}

// --- Tahap 3: checkpoint konteks

// textAndTool is a reply that says something and then calls a tool, the way
// a model closes a step: "Config fixed and verified." + todo(...).
func textAndTool(text, name, args string) string {
	return sse(
		delta(map[string]any{"content": text}),
		delta(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "c1", "function": map[string]any{"name": name, "arguments": args}}}}),
	)
}

func TestCheckpointLeavesCleanContext(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		textReply("hai"), // giliran pertama, harus tetap ada setelah checkpoint
		toolReply("todo", `{"items": [{"text": "fix config", "status": "in_progress"}, {"text": "add flag", "status": "pending"}]}`),
		toolReply("read_file", `{"path": "a.txt"}`),
		toolReply("write_file", `{"path": "config.yaml", "content": "a: 1"}`),
		toolReply("bash", `{"command": "test -f config.yaml"}`),
		textAndTool("Config fixed and verified.", "todo", `{"items": [{"text": "fix config", "status": "done"}, {"text": "add flag", "status": "in_progress"}]}`),
		// setelah checkpoint: langkah baca saja, tidak ada mutasi -> tanpa checkpoint kedua
		toolReply("todo", `{"items": [{"text": "fix config", "status": "done"}, {"text": "add flag", "status": "done"}]}`),
		textReply("Selesai."),
	)
	a.RunTurn(context.Background(), "halo")
	a.RunTurn(context.Background(), "perbaiki config lalu tambah flag")

	// Permintaan ke-7 (indeks 6) adalah yang pertama setelah checkpoint.
	msgs, _ := f.reqs[6]["messages"].([]any)
	if len(msgs) != 4 { // system, "halo", "hai", permintaan giliran ini
		t.Fatalf("setelah checkpoint konteks harus tinggal permintaan asli: %d pesan", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	last, _ := msgs[3].(map[string]any)["content"].(string)
	if last != "perbaiki config lalu tambah flag" {
		t.Fatalf("permintaan asli hilang: %q", last)
	}
	for _, want := range []string{"[checkpoints this turn", "done: fix config", "changed: config.yaml", "passed: test -f config.yaml", "note: Config fixed and verified.", "already read this turn", "a.txt", "[x] fix config", "[>] add flag"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("handoff kehilangan %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "already attempted") {
		t.Fatal("ledger harus kosong setelah checkpoint: buktinya sudah tidak ada di konteks")
	}
	for _, m := range msgs[1:] { // pesan sistem memuat contoh "exit_code" di basePrompt
		if c, _ := m.(map[string]any)["content"].(string); strings.Contains(c, "Plan updated") || strings.Contains(c, "exit_code") {
			t.Fatal("transkrip jendela lama tidak boleh ikut ke jendela baru")
		}
	}
	if n := strings.Count(screen.String(), "checkpoint:"); n != 1 {
		t.Fatalf("langkah baca tanpa mutasi tidak boleh memicu checkpoint: %d", n)
	}
	if len(a.todos) != 2 || a.todos[1].Status != "done" {
		t.Fatalf("rencana setelah giliran: %+v", a.todos)
	}
}

// --- Gambar: lampiran disebut di teks, read_file pada gambar melampirkannya

// partsOf returns a request message's content parts (an image message) or nil.
func partsOf(m map[string]any) []map[string]any {
	raw, _ := m["content"].([]any)
	var out []map[string]any
	for _, p := range raw {
		pm, _ := p.(map[string]any)
		out = append(out, pm)
	}
	return out
}

func TestAttachedImageIsNamedInText(t *testing.T) {
	a, f, _, _ := newFakeAgent(t, textReply("Saya lihat burung kuning."))
	a.Attach(Image{Mime: "image/png", Data: onePixelPNG, Name: "shot.png"})
	a.RunTurn(context.Background(), "apa yang salah di game ini?")
	parts := partsOf(msgsOf(f.reqs[0])[1])
	if len(parts) != 2 || parts[1]["type"] != "image_url" {
		t.Fatalf("pesan user harus teks + gambar: %v", parts)
	}
	if text, _ := parts[0]["text"].(string); !strings.Contains(text, "[attached image: shot.png") || !strings.Contains(text, "apa yang salah") {
		t.Fatalf("lampiran harus disebut namanya di teks: %q", text)
	}
}

func TestReadFileOnImageAttachesIt(t *testing.T) {
	a, f, dir, _ := newFakeAgent(t,
		toolReply("read_file", `{"path": "shot.png"}`),
		textReply("Burungnya tidak kelihatan karena warnanya sama dengan latar."),
	)
	os.WriteFile(filepath.Join(dir, "shot.png"), onePixelPNG, 0o644)
	a.RunTurn(context.Background(), "lihat shot.png")
	tm := toolMsgs(a)
	if len(tm) != 1 || !strings.Contains(tm[0].Content, "attached") || strings.Contains(tm[0].Content, "binary") {
		t.Fatalf("read_file pada gambar harus melampirkan, bukan menolak: %q", tm[0].Content)
	}
	// Permintaan berikutnya: hasil tool, lalu pesan harness yang membawa gambarnya.
	msgs := msgsOf(f.reqs[1])
	last := msgs[len(msgs)-1]
	parts := partsOf(last)
	if last["role"] != "user" || len(parts) != 2 || parts[1]["type"] != "image_url" {
		t.Fatalf("gambar harus dikirim sebagai pesan user setelah hasil tool: %v", last)
	}
	if msgs[len(msgs)-2]["role"] != "tool" {
		t.Fatal("hasil tool harus tetap bersambung di belakang pesan asisten")
	}
}

func TestBudgetEndReportsUnfinishedWork(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		toolReply("write_file", `{"path": "config.yaml", "content": "a: 1"}`),
		toolReply("bash", `{"command": "echo masih kerja"}`), // langkah ke-2: anggaran habis di tengah
		textReply("Lanjut."),
	)
	a.cfg.MaxSteps = 2
	a.env.VerifyCmds = []string{"test -f config.yaml"}
	a.RunTurn(context.Background(), "ubah config")
	for _, want := range []string{"batas 2 langkah", "file yang diubah giliran ini: config.yaml", "harness menjalankan test -f config.yaml", "verifikasi lulus"} {
		if !strings.Contains(screen.String(), want) {
			t.Fatalf("laporan akhir giliran kehilangan %q:\n%s", want, screen)
		}
	}
	if strings.Contains(screen.String(), "coba: /task ubah") {
		t.Fatal("saran /task tidak boleh menggema pesan user yang terpotong")
	}
	// Giliran berikutnya mulai dari kenyataan: catatan dibawa di pesan user.
	a.RunTurn(context.Background(), "lanjutkan")
	msgs := msgsOf(f.reqs[len(f.reqs)-1])
	last := content(msgs[len(msgs)-1])
	if !strings.Contains(last, "ran out of steps") || !strings.Contains(last, "config.yaml") || !strings.Contains(last, "lanjutkan") {
		t.Fatalf("catatan harus dibawa ke giliran berikutnya dalam satu pesan: %q", last)
	}
	if a.carry != "" {
		t.Fatal("catatan harus dipakai sekali lalu dikosongkan")
	}
}

// --- Narasi tanpa aksi: balasan yang mengumumkan langkah tidak menutup giliran

func TestLooksLikeIntent(t *testing.T) {
	yes := []string{
		"Mari saya perbaiki logika collision detection.",
		"Saya akan memperbaiki bug collision detection. Masalahnya kemungkinan bounding box terlalu besar.",
		"Let me read the file first.",
		"I'll fix that now.",
		"Analisis selesai.\n\nSekarang saya akan mengubah App.jsx.",
	}
	no := []string{
		"Fungsi itu menghitung jumlah kata dengan memisahkan spasi.",
		"Sudah diperbaiki: bounding box burung dikecilkan 4px dan npm test lulus.",
		"Anda bisa menjalankan `npm run dev` untuk mencobanya.",
	}
	for _, s := range yes {
		if !looksLikeIntent(s) {
			t.Errorf("harus dikenali sebagai niat: %q", s)
		}
	}
	for _, s := range no {
		if looksLikeIntent(s) {
			t.Errorf("bukan niat: %q", s)
		}
	}
}

func TestIntentWithoutActionIsNudged(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		textReply("Mari saya perbaiki logika collision detection."), // niat tanpa tool
		toolReply("read_file", `{"path": "a.txt"}`),
		textReply("Sudah saya periksa: isinya hanya sapaan, tidak ada bug."),
	)
	a.RunTurn(context.Background(), "ada bug nih")
	if len(f.reqs) != 3 {
		t.Fatalf("niat tanpa aksi harus ditegur sekali lalu dilanjutkan: %d permintaan", len(f.reqs))
	}
	var nudged bool
	for _, m := range a.msgs {
		if m.Role == "user" && strings.Contains(m.Content, "called no tool") {
			nudged = true
		}
	}
	if !nudged || !strings.Contains(screen.String(), "mengumumkan langkah") {
		t.Fatalf("teguran tidak terkirim:\n%s", screen)
	}

	// Jawaban akhir yang sungguhan tidak ditegur, dan teguran hanya sekali.
	a, f, _, _ = newFakeAgent(t, textReply("Fungsi itu menghitung jumlah kata."))
	a.RunTurn(context.Background(), "apa fungsi ini?")
	if len(f.reqs) != 1 {
		t.Fatalf("jawaban biasa tidak boleh ditegur: %d permintaan", len(f.reqs))
	}
	// Teguran niat hanya sekali; niat yang diulang persis lalu ditangani
	// penjaga balasan-identik (tool call saja), dan jawaban berbeda diterima.
	a, f, _, _ = newFakeAgent(t, textReply("Saya akan memperbaikinya."), textReply("Saya akan memperbaikinya."), textReply("Tidak ada yang perlu diubah."))
	a.RunTurn(context.Background(), "perbaiki")
	if len(f.reqs) != 3 {
		t.Fatalf("niat → identik → jawaban: 3 permintaan, dapat %d", len(f.reqs))
	}
	var intent, identical int
	for _, m := range a.msgs {
		if m.Role == "user" && strings.Contains(m.Content, "called no tool") {
			intent++
		}
		if m.Role == "user" && strings.Contains(m.Content, "identical") {
			identical++
		}
	}
	if intent != 1 || identical != 1 {
		t.Fatalf("teguran niat %d× (mau 1), teguran identik %d× (mau 1)", intent, identical)
	}
}

func TestTagTextToolCallsAreExecuted(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		textReply("Saya akan memeriksa filenya.\n\n<read_file> {\"path\": \"a.txt\"}"),
		textReply("Sudah: isinya hello."),
	)
	a.RunTurn(context.Background(), "cek a.txt")
	tm := toolMsgs(a)
	if len(tm) != 1 || !strings.Contains(tm[0].Content, "hello") {
		t.Fatalf("tag teks harus dieksekusi sebagai tool call: %+v", tm)
	}
	if len(f.reqs) != 2 || strings.Contains(screen.String(), "mengumumkan langkah") {
		t.Fatalf("panggilan yang dikenali tidak boleh ditegur sebagai niat kosong (%d permintaan):\n%s", len(f.reqs), screen)
	}
	// read_file pada folder mengarahkan ke list_files, bukan error mentah.
	a, _, _, _ = newFakeAgent(t, toolReply("read_file", `{"path": "."}`), textReply("ok"))
	a.RunTurn(context.Background(), "baca folder")
	if tm := toolMsgs(a); len(tm) != 1 || !strings.Contains(tm[0].Content, "list_files") {
		t.Fatalf("read_file pada folder: %+v", tm)
	}
}

// --- Log 15-19-23: balasan identik berulang, dan gambar yang mematikan tool call

func TestIdenticalRepliesStallTheTurn(t *testing.T) {
	same := textReply("Mari saya lihat struktur folder game dan file-file JavaScript-nya.")
	a, f, _, screen := newFakeAgent(t, same, same, same, textReply("tidak tercapai"))
	a.RunTurn(context.Background(), "cek bug")
	if len(f.reqs) != 3 {
		t.Fatalf("niat → identik → identik harus berhenti di 3 permintaan, dapat %d", len(f.reqs))
	}
	var stall bool
	for _, m := range a.msgs {
		if m.Role == "user" && strings.Contains(m.Content, "identical") {
			stall = true
		}
	}
	if !stall || !strings.Contains(screen.String(), "mengulang balasan yang sama 3x") {
		t.Fatalf("balasan identik harus ditegur lalu dihentikan, bukan diterima sebagai jawaban:\n%s", screen)
	}
	// Setelah teguran, thinking harus menyala: model tanpa reasoning cuma mengulang.
	if got := thinkFlags(f); !got[1] || !got[2] {
		t.Fatalf("thinking per request = %v, mau nyala setelah teguran", got)
	}
}

func TestImageSentOnlyOnRequestAfterAttach(t *testing.T) {
	a, f, _, _ := newFakeAgent(t,
		toolReply("read_file", `{"path": "a.txt"}`),
		textReply("Selesai."),
	)
	a.Attach(Image{Mime: "image/png", Data: onePixelPNG, Name: "shot.png"})
	a.RunTurn(context.Background(), "lihat ini")
	if parts := partsOf(msgsOf(f.reqs[0])[1]); len(parts) != 2 || parts[1]["type"] != "image_url" {
		t.Fatalf("request pertama harus membawa gambarnya: %v", parts)
	}
	m := msgsOf(f.reqs[1])[1]
	if _, isParts := m["content"].([]any); isParts {
		t.Fatal("request berikutnya tidak boleh membawa bagian gambar lagi")
	}
	if c := content(m); !strings.Contains(c, "[image shot.png was shown to you earlier") || !strings.Contains(c, "lihat ini") {
		t.Fatalf("gambar harus diganti catatan teks: %q", c)
	}
	// Riwayat asli tetap menyimpan gambarnya (fitContext yang memutuskan nasibnya).
	if len(a.msgs[a.turnStart].Images) != 1 {
		t.Fatal("a.msgs tidak boleh diubah, hanya salinan yang dikirim")
	}
}

func TestBareTextToolCallsAreExecuted(t *testing.T) {
	a, f, _, screen := newFakeAgent(t,
		textReply("Let me read the files.\n\nread_file {\"path\": \"a.txt\"}\nlist_files {\"path\": \".\"}"),
		textReply("Sudah: a.txt berisi hello."),
	)
	a.RunTurn(context.Background(), "cek")
	tm := toolMsgs(a)
	if len(tm) != 2 || !strings.Contains(tm[0].Content, "hello") || !strings.Contains(tm[1].Content, "a.txt") {
		t.Fatalf("baris telanjang harus dieksekusi sebagai dua tool call: %+v", tm)
	}
	if len(f.reqs) != 2 || strings.Contains(screen.String(), "mengumumkan langkah") {
		t.Fatalf("panggilan yang dikenali tidak boleh ditegur (%d permintaan):\n%s", len(f.reqs), screen)
	}
}
