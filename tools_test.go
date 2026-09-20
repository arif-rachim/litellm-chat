package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateArgs(t *testing.T) {
	edit := toolByName("edit_file")
	args, problem := validateArgs(edit, map[string]any{"file_path": "a.js", "old": "x", "new_str": "y"})
	if problem != "" || args["path"] != "a.js" || args["old_string"] != "x" || args["new_string"] != "y" {
		t.Fatalf("aliases: %v %q", args, problem)
	}
	_, problem = validateArgs(edit, map[string]any{"path": "a.js"})
	if !strings.Contains(problem, "old_string") || !strings.Contains(problem, "new_string") {
		t.Fatalf("missing args not reported: %q", problem)
	}
	args, problem = validateArgs(toolByName("read_file"), map[string]any{"path": "a", "offset": "10"})
	if problem != "" || args["offset"] != float64(10) {
		t.Fatalf("numeric string not coerced: %v %q", args, problem)
	}
	args, problem = validateArgs(toolByName("write_file"), map[string]any{"path": "p.json", "content": map[string]any{"a": 1.0}})
	if problem != "" || !strings.Contains(args["content"].(string), `"a": 1`) {
		t.Fatalf("object content not serialised: %v %q", args, problem)
	}
	args, problem = validateArgs(toolByName("todo"), map[string]any{"todos": `[{"text":"a","status":"done"}]`})
	if problem != "" || len(args["items"].([]any)) != 1 {
		t.Fatalf("todo as JSON string: %v %q", args, problem)
	}
	if normalizeToolName("functions.Shell") != "bash" || normalizeToolName("str_replace") != "edit_file" {
		t.Fatal("tool name aliases")
	}
}

func testAgentIn(t *testing.T, dir string) *Agent {
	cfg := &Config{BaseURL: "http://127.0.0.1:1", Model: "qwen3.5-test", ToolMax: 8000, MaxSteps: 12, Think: "auto", Yolo: true}
	ui := NewUI(&strings.Builder{}, &strings.Builder{}, false, false, false)
	return NewAgent(cfg, ui, nil, nil, nil, dir)
}

func TestEditFile(t *testing.T) {
	dir := t.TempDir()
	a := testAgentIn(t, dir)
	path := filepath.Join(dir, "m.js")
	os.WriteFile(path, []byte("function f() {\n  return a - b;\n}\nconst x = 1;\nconst x2 = 1;\n"), 0o644)
	ctx := context.Background()

	res := runEdit(ctx, a, map[string]any{"path": "m.js", "old_string": "return a - b;", "new_string": "return a + b;"})
	if res.Invalid || res.Failed || res.Mutated == "" {
		t.Fatalf("edit failed: %+v", res)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "return a + b;") {
		t.Fatal("file not changed")
	}
	if !res.CheckOK && hasNode() {
		t.Fatalf("node --check should pass: %v", res.Detail)
	}

	res = runEdit(ctx, a, map[string]any{"path": "m.js", "old_string": "return a * b;", "new_string": "x"})
	if !res.Invalid || !strings.Contains(res.Output, "Snippet") || !strings.Contains(res.Output, "return a + b;") {
		t.Fatalf("no-match should show the nearest snippet:\n%s", res.Output)
	}
	res = runEdit(ctx, a, map[string]any{"path": "m.js", "old_string": "\treturn a + b;", "new_string": "x"})
	if !strings.Contains(res.Output, "indentation") {
		t.Fatalf("whitespace mismatch should be explained:\n%s", res.Output)
	}
	res = runEdit(ctx, a, map[string]any{"path": "m.js", "old_string": " = 1;", "new_string": " = 2;"})
	if !res.Invalid || !strings.Contains(res.Output, "2 times") || !strings.Contains(res.Output, "lines 4, 5") {
		t.Fatalf("multiple matches should list lines:\n%s", res.Output)
	}

	// A broken edit is saved but reported by the auto-check.
	if hasNode() {
		res = runEdit(ctx, a, map[string]any{"path": "m.js", "old_string": "const x = 1;", "new_string": "const x = ;"})
		if !res.Failed || !strings.Contains(res.Output, "Auto-check FAILED") {
			t.Fatalf("syntax error not caught:\n%s", res.Output)
		}
	}
}

func hasNode() bool {
	_, err := os.Stat("/usr/bin/node")
	if err == nil {
		return true
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(d, "node")); err == nil {
			return true
		}
	}
	return false
}

func TestReadMissingFileSuggestsSimilar(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "index.js"), []byte("x\n"), 0o644)
	a := testAgentIn(t, dir)
	res := runRead(context.Background(), a, map[string]any{"path": "index.js"})
	if !res.Failed || !strings.Contains(res.Output, "src/index.js") {
		t.Fatalf("expected a hint to src/index.js:\n%s", res.Output)
	}
}

func TestBash(t *testing.T) {
	a := testAgentIn(t, t.TempDir())
	res := runBash(context.Background(), a, map[string]any{"command": "echo hi; exit 3"})
	if !res.Failed || !strings.HasPrefix(res.Output, "exit_code: 3\nhi") {
		t.Fatalf("got %+v", res)
	}
	res = runBash(context.Background(), a, map[string]any{"command": "sleep 5", "timeout_sec": float64(1)})
	if !res.Failed || !strings.Contains(res.Output, "timed out") {
		t.Fatalf("timeout: %+v", res)
	}
	if !runBash(context.Background(), a, map[string]any{"command": "npm test || true"}).Verify {
		t.Fatal("npm test should count as verification")
	}
}

func TestTruncate(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		sb.WriteString("line of output\n")
	}
	out := truncate(sb.String(), 1000)
	if len(out) > 1100 || !strings.Contains(out, "lines truncated") || !strings.HasPrefix(out, "line of output") {
		t.Fatalf("bad truncation (%d bytes): %q", len(out), out[:80])
	}
	if truncate("short", 1000) != "short" {
		t.Fatal("short text changed")
	}
}

func TestTodo(t *testing.T) {
	a := testAgentIn(t, t.TempDir())
	res := runTodo(context.Background(), a, map[string]any{"items": []any{
		map[string]any{"content": "read", "status": "completed"},
		map[string]any{"text": "fix", "status": "in-progress"},
		"test",
	}})
	if res.Invalid || len(a.todos) != 3 || a.todos[0].Status != "done" || a.todos[1].Status != "in_progress" || a.todos[2].Status != "pending" {
		t.Fatalf("got %+v %+v", res, a.todos)
	}
	if open := openTodos(a.todos); len(open) != 2 {
		t.Fatalf("open todos: %v", open)
	}
}

func TestDotEnv(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.env"), filepath.Join(dir, "b.env")
	os.WriteFile(a, []byte("# comment\nexport LCHAT_MODEL=\"from-a\"\nOPENROUTER_API_KEY='k1'\nAWS_SECRET=nope\n"), 0o600)
	os.WriteFile(b, []byte("LCHAT_MODEL=from-b\nLCHAT_CTX=1234\n"), 0o600)
	dotenv = map[string]string{}
	t.Cleanup(func() { dotenv = map[string]string{} })
	loaded := loadDotEnv(a, filepath.Join(dir, "missing"), b)
	if len(loaded) != 2 || dotenv["LCHAT_MODEL"] != "from-a" || dotenv["OPENROUTER_API_KEY"] != "k1" || dotenv["LCHAT_CTX"] != "1234" {
		t.Fatalf("dotenv = %v (loaded %v)", dotenv, loaded)
	}
	if _, ok := dotenv["AWS_SECRET"]; ok {
		t.Fatal("unrelated secrets must not be read")
	}
	if os.Getenv("OPENROUTER_API_KEY") == "k1" {
		t.Fatal(".env must not leak into the process environment")
	}
	t.Setenv("LCHAT_API_KEY", "secret")
	for _, kv := range childEnv() {
		if strings.HasPrefix(kv, "LCHAT_API_KEY=") {
			t.Fatal("API key passed to agent commands")
		}
	}
}
