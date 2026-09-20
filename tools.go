package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

type Param struct {
	Name, Type, Desc string
	Items            map[string]any // schema of array items
}

type Tool struct {
	Name     string
	Desc     string
	Params   []Param
	Required []string
	Example  string // example arguments, shown to the model in corrections
	Perm     bool   // needs user permission
	Run      func(ctx context.Context, a *Agent, args map[string]any) ToolResult
}

// ToolResult is what a tool returns: Output goes to the model, the rest drives
// the screen and the harness.
type ToolResult struct {
	Output  string
	Failed  bool   // ran but failed (non-zero exit, broken file): triggers reflection
	Invalid bool   // malformed call: a harness correction
	Denied  bool   // user said no
	ErrKey  string // identifies an invalid-call kind for the retry limit
	Summary string // one-line call description ("bash npm test")
	Status  string // screen: "exit 0 · 1.2s · 34 baris"
	Detail  []string
	Diff    []string
	Mutated string // path written or edited
	CheckOK bool   // auto-check ran and passed
	Verify  bool   // bash command that counts as verification
	Image   *Image // an image for the model to look at, sent right after this batch
}

var ignoredDirs = map[string]bool{
	"node_modules": true, ".git": true, "dist": true, "build": true, ".next": true,
	"__pycache__": true, ".venv": true, "venv": true, ".cache": true, "coverage": true, "target": true,
}

var tools = []*Tool{
	{
		Name: "bash",
		Desc: "Run a bash command in the working directory (non-interactive, no stdin). Returns exit_code and combined stdout/stderr.",
		Params: []Param{
			{Name: "command", Type: "string", Desc: "The command to run"},
			{Name: "timeout_sec", Type: "integer", Desc: "Timeout in seconds (default 120, max 600)"},
		},
		Required: []string{"command"},
		Example:  `{"command": "npm test"}`,
		Perm:     true,
		Run:      runBash,
	},
	{
		Name: "read_file",
		Desc: "Read a text file. Output has line numbers (not part of the file).",
		Params: []Param{
			{Name: "path", Type: "string", Desc: "File path, relative to the working directory"},
			{Name: "offset", Type: "integer", Desc: "First line to read, 1-based (default 1)"},
			{Name: "limit", Type: "integer", Desc: "Number of lines (default 400)"},
		},
		Required: []string{"path"},
		Example:  `{"path": "src/index.js"}`,
		Run:      runRead,
	},
	{
		Name: "list_files",
		Desc: "List files recursively (skips node_modules, .git, dist, build...).",
		Params: []Param{
			{Name: "path", Type: "string", Desc: "Directory (default .)"},
			{Name: "pattern", Type: "string", Desc: "Optional glob on file names, e.g. *.test.js"},
		},
		Example: `{"path": "src", "pattern": "*.js"}`,
		Run:     runList,
	},
	{
		Name: "write_file",
		Desc: "Create or overwrite a file with the full content. For small changes to an existing file use edit_file.",
		Params: []Param{
			{Name: "path", Type: "string", Desc: "File path"},
			{Name: "content", Type: "string", Desc: "Full file content"},
		},
		Required: []string{"path", "content"},
		Example:  `{"path": "src/util.js", "content": "export const add = (a, b) => a + b;\n"}`,
		Perm:     true,
		Run:      runWrite,
	},
	{
		Name: "edit_file",
		Desc: "Replace one exact occurrence of old_string with new_string in a file. old_string must match the file exactly (including indentation) and be unique; include surrounding lines to make it unique.",
		Params: []Param{
			{Name: "path", Type: "string", Desc: "File path"},
			{Name: "old_string", Type: "string", Desc: "Exact text to replace"},
			{Name: "new_string", Type: "string", Desc: "Replacement text"},
		},
		Required: []string{"path", "old_string", "new_string"},
		Example:  `{"path": "src/math.js", "old_string": "return a - b;", "new_string": "return a + b;"}`,
		Perm:     true,
		Run:      runEdit,
	},
	{
		Name: "ask_user",
		Desc: "Ask the user a question and wait for the answer. Use it when the decision is the user's (which approach or library, which file, what the behaviour should be) or when the request is ambiguous. Do not use it for anything you can find out yourself by reading files or running a command.",
		Params: []Param{
			{Name: "question", Type: "string", Desc: "One clear question"},
			{Name: "options", Type: "array", Desc: "Optional short choices to pick from", Items: map[string]any{"type": "string"}},
		},
		Required: []string{"question"},
		Example:  `{"question": "Pakai library form yang mana?", "options": ["react-hook-form", "formik", "tanpa library"]}`,
		Run:      runAsk,
	},
	{
		Name: "todo",
		Desc: "Set your task plan (replaces the previous list). Use it for tasks with more than 2 steps and update statuses as you work.",
		Params: []Param{
			{Name: "items", Type: "array", Desc: "The full list of steps", Items: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text":   map[string]any{"type": "string"},
					"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "done"}},
				},
				"required": []string{"text", "status"},
			}},
		},
		Required: []string{"items"},
		Example:  todoExample,
		Run:      runTodo,
	},
}

// toolByName looks in both tool sets, blind to scope, so a call to a tool
// that exists but is out of scope gets a correction about scope rather than
// "no such tool".
func toolByName(name string) *Tool {
	for _, t := range append(append([]*Tool{}, tools...), taskTools...) {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func isTaskTool(name string) bool {
	for _, t := range taskTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// activeTools is the tool set the model may call: inside a subtask, todo is
// hidden (the harness owns the plan) and subtask_done appears.
func activeTools(task bool) []*Tool {
	if !task {
		return tools
	}
	var out []*Tool
	for _, t := range tools {
		if t.Name != "todo" {
			out = append(out, t)
		}
	}
	return append(out, taskTools...)
}

func toolNames() []string {
	var n []string
	for _, t := range tools {
		n = append(n, t.Name)
	}
	return n
}

var toolAliases = map[string]string{
	"shell": "bash", "run": "bash", "run_command": "bash", "execute": "bash", "exec": "bash", "terminal": "bash", "sh": "bash",
	"read": "read_file", "cat": "read_file", "view": "read_file", "open_file": "read_file", "view_file": "read_file",
	"write": "write_file", "create_file": "write_file", "save_file": "write_file",
	"edit": "edit_file", "replace": "edit_file", "str_replace": "edit_file", "replace_in_file": "edit_file",
	"ls": "list_files", "list": "list_files", "list_dir": "list_files", "glob": "list_files", "find_files": "list_files",
	"todos": "todo", "todo_write": "todo", "todowrite": "todo", "plan": "todo", "update_todo": "todo", "update_todos": "todo",
}

func normalizeToolName(n string) string {
	n = strings.ToLower(strings.TrimSpace(n))
	if i := strings.LastIndexAny(n, ".:"); i >= 0 { // "functions.bash", "tools:bash"
		n = n[i+1:]
	}
	if a, ok := toolAliases[n]; ok {
		return a
	}
	return n
}

var argAliases = map[string]string{
	"file_path": "path", "filepath": "path", "file": "path", "filename": "path", "file_name": "path", "dir": "path", "directory": "path",
	"cmd": "command", "script": "command", "shell": "command", "code": "command",
	"old": "old_string", "old_str": "old_string", "oldstring": "old_string", "search": "old_string", "find": "old_string", "old_text": "old_string",
	"new": "new_string", "new_str": "new_string", "newstring": "new_string", "replacement": "new_string", "new_text": "new_string",
	"text": "content", "contents": "content", "data": "content", "body": "content", "file_content": "content",
	"timeout": "timeout_sec", "todos": "items", "tasks": "items", "steps": "items", "glob": "pattern",
	"start_line": "offset", "line": "offset", "lines": "limit", "max_lines": "limit",
}

func toolSchemas(task bool) []any {
	var out []any
	for _, t := range activeTools(task) {
		props := map[string]any{}
		for _, p := range t.Params {
			s := map[string]any{"type": p.Type, "description": p.Desc}
			if p.Items != nil {
				s["items"] = p.Items
			}
			props[p.Name] = s
		}
		req := t.Required
		if req == nil {
			req = []string{}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Desc,
				"parameters":  map[string]any{"type": "object", "properties": props, "required": req},
			},
		})
	}
	return out
}

func (t *Tool) signature() string {
	var parts []string
	for _, p := range t.Params {
		opt := "?"
		for _, r := range t.Required {
			if r == p.Name {
				opt = ""
			}
		}
		parts = append(parts, p.Name+opt+": "+p.Type)
	}
	return t.Name + "(" + strings.Join(parts, ", ") + ")"
}

// validateArgs maps aliases, coerces types and checks required arguments. It
// returns the cleaned args, or a description of what is wrong.
func validateArgs(t *Tool, in map[string]any) (map[string]any, string) {
	known := map[string]*Param{}
	for i := range t.Params {
		known[t.Params[i].Name] = &t.Params[i]
	}
	args := map[string]any{}
	for k, v := range in {
		if _, ok := known[k]; !ok {
			if a, ok := argAliases[strings.ToLower(k)]; ok && known[a] != nil {
				if _, dup := in[a]; !dup {
					k = a
				}
			}
		}
		if _, ok := known[k]; ok {
			args[k] = v
		}
	}
	var problems []string
	for k, v := range args {
		p := known[k]
		switch p.Type {
		case "string":
			switch x := v.(type) {
			case string:
			case float64, bool:
				args[k] = fmt.Sprint(x)
			case nil:
				delete(args, k)
			default: // e.g. JSON file content given as an object
				b, _ := json.MarshalIndent(x, "", "  ")
				args[k] = string(b) + "\n"
			}
		case "integer":
			switch x := v.(type) {
			case float64:
			case string:
				n, err := strconv.Atoi(strings.TrimSpace(x))
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s must be an integer, got %q", k, x))
					continue
				}
				args[k] = float64(n)
			case nil:
				delete(args, k)
			default:
				problems = append(problems, fmt.Sprintf("%s must be an integer", k))
			}
		case "array":
			if s, ok := v.(string); ok {
				var arr []any
				if err := json.Unmarshal([]byte(s), &arr); err != nil {
					problems = append(problems, fmt.Sprintf("%s must be a JSON array", k))
					continue
				}
				args[k] = arr
			} else if _, ok := v.([]any); !ok {
				problems = append(problems, fmt.Sprintf("%s must be an array", k))
			}
		}
	}
	for _, r := range t.Required {
		if _, ok := args[r]; !ok {
			problems = append(problems, "missing required argument "+r)
		}
	}
	if len(problems) > 0 {
		return nil, strings.Join(problems, "; ")
	}
	return args, ""
}

func str(args map[string]any, k string) string { s, _ := args[k].(string); return s }

func num(args map[string]any, k string, def int) int {
	if f, ok := args[k].(float64); ok {
		return int(f)
	}
	return def
}

func (a *Agent) resolve(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(a.cwd, p)
	}
	return filepath.Clean(p)
}

func (a *Agent) rel(p string) string {
	if r, err := filepath.Rel(a.cwd, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

// truncate keeps the head and tail of long output.
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes || maxBytes <= 0 {
		return s
	}
	headN, tailN := maxBytes*2/3, maxBytes/3
	head := s[:headN]
	if i := strings.LastIndexByte(head, '\n'); i > headN/2 {
		head = head[:i+1]
	}
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	tail := s[len(s)-tailN:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < tailN/2 {
		tail = tail[i+1:]
	}
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	omitted := strings.Count(s[len(head):len(s)-len(tail)], "\n")
	return head + fmt.Sprintf("\n...[%d lines truncated]...\n", omitted) + tail
}

func lastLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		if len(l) > 160 {
			lines[i] = l[:160] + "…"
		}
	}
	return lines
}

// capBuffer keeps at most max bytes (enough for truncate to work with).
type capBuffer struct {
	buf bytes.Buffer
	max int
	cut bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.cut = true
		} else {
			c.buf.Write(p)
		}
	} else {
		c.cut = true
	}
	return len(p), nil
}

// childEnv is the environment for commands the agent runs, without lchat's
// own settings and API keys.
func childEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !dotenvKey(k) {
			env = append(env, kv)
		}
	}
	return env
}

func runBash(ctx context.Context, a *Agent, args map[string]any) ToolResult {
	command := str(args, "command")
	timeout := time.Duration(min(max(num(args, "timeout_sec", 120), 1), 600)) * time.Second
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "bash", "-c", command)
	cmd.Dir = a.cwd
	cmd.Env = append(childEnv(), "CI=true", "PAGER=cat", "GIT_PAGER=cat", "NO_COLOR=1", "FORCE_COLOR=0", "TERM=dumb")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	out := &capBuffer{max: 4 << 20}
	cmd.Stdout, cmd.Stderr = out, out
	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start).Seconds()

	code := 0
	var note string
	var ee *exec.ExitError
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		code = -1
		note = fmt.Sprintf("\n[harness] The command timed out after %ds and was killed. If it starts a server or watch mode, run it in the background (`cmd > /tmp/out.log 2>&1 &`) or use a one-shot flag.", int(timeout.Seconds()))
	case ctx.Err() != nil:
		code = -1
		note = "\n[harness] Cancelled by the user."
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		code = -1
		note = "\n" + err.Error()
	}
	text := out.buf.String()
	lines := strings.Count(text, "\n")
	if text == "" {
		text = "(no output)"
	}
	res := ToolResult{
		Output: fmt.Sprintf("exit_code: %d\n%s%s", code, truncate(text, a.cfg.ToolMax), note),
		Failed: code != 0,
		Verify: isVerifyCmd(command, a.env.VerifyCmds),
		Status: fmt.Sprintf("exit %d · %.1fs · %d baris", code, dur, lines),
	}
	if strings.TrimSpace(out.buf.String()) != "" {
		n := 3
		if res.Failed {
			n = 6
		}
		if a.cfg.Verbose {
			n = 10000
		}
		res.Detail = lastLines(out.buf.String(), n)
	}
	return res
}

func runRead(ctx context.Context, a *Agent, args map[string]any) ToolResult {
	path := a.resolve(str(args, "path"))
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		return ToolResult{Invalid: true, ErrKey: "read_dir", Status: "itu folder, bukan file", Output: correction(
			"read_file: "+a.rel(path)+" is a directory.",
			"read_file reads one text file.",
			"Use list_files to see what is inside, then read_file on a file.", `list_files {"path": "`+a.rel(path)+`"}`)}
	}
	if imageExt[strings.ToLower(filepath.Ext(path))] != "" {
		// Reading an image is the model asking to see it. Refusing with
		// "binary file" sends it off to analyze pixels with scripts; attaching
		// the image answers the real question in one step.
		img, err := loadImage(path)
		if err != nil {
			return a.fileError("read_file", path, err)
		}
		return ToolResult{
			Output: "This is an image, not text. It is attached to the message right after this result: look at it directly and describe what you see. Do not analyze it with scripts.",
			Status: "gambar dilampirkan · " + img.label(),
			Image:  &img,
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return a.fileError("read_file", path, err)
	}
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return ToolResult{Output: "This is a binary file; it cannot be read as text.", Failed: true, Status: "file biner"}
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	off := max(num(args, "offset", 1), 1)
	lim := max(num(args, "limit", 400), 1)
	if off > len(lines) && len(lines) > 0 {
		return ToolResult{Output: fmt.Sprintf("offset %d is past the end: the file has %d lines.", off, len(lines)), Failed: true, Status: "offset di luar file"}
	}
	end := min(off-1+lim, len(lines))
	var sb strings.Builder
	for i := off - 1; i < end; i++ {
		l := lines[i]
		if len(l) > 500 {
			l = l[:500] + "…[line truncated]"
		}
		fmt.Fprintf(&sb, "%5d\t%s\n", i+1, l)
	}
	if len(lines) == 0 {
		sb.WriteString("(empty file)\n")
	}
	if end < len(lines) {
		fmt.Fprintf(&sb, "... %d more lines (use offset=%d to continue)\n", len(lines)-end, end+1)
	}
	a.readFiles[path] = true
	return ToolResult{
		Output: truncate(sb.String(), a.cfg.ToolMax*2),
		Status: fmt.Sprintf("baris %d-%d dari %d", min(off, len(lines)), end, len(lines)),
	}
}

func (a *Agent) fileError(tool, path string, err error) ToolResult {
	if errors.Is(err, fs.ErrNotExist) {
		hint := ""
		if sim := a.similarFiles(filepath.Base(path)); len(sim) > 0 {
			hint = " Similar files: " + strings.Join(sim, ", ") + "."
		}
		return ToolResult{
			Output: correction(fmt.Sprintf("%s: %s does not exist.", tool, a.rel(path)),
				"The path is relative to the working directory "+a.cwd+"."+hint,
				"Check the path with list_files, or create the file with write_file.", ""),
			Failed: true, Status: "file tidak ada",
		}
	}
	return ToolResult{Output: fmt.Sprintf("%s: %v", tool, err), Failed: true, Status: err.Error()}
}

// similarFiles finds up to 3 files with the same base name (case-insensitive).
func (a *Agent) similarFiles(base string) []string {
	var out []string
	base = strings.ToLower(base)
	_ = filepath.WalkDir(a.cwd, func(p string, d fs.DirEntry, err error) error {
		if len(out) >= 3 {
			return fs.SkipAll
		}
		if err != nil {
			return nil
		}
		if d.IsDir() && ignoredDirs[d.Name()] {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.ToLower(d.Name()) == base {
			out = append(out, a.rel(p))
		}
		return nil
	})
	return out
}

func runList(ctx context.Context, a *Agent, args map[string]any) ToolResult {
	root := a.resolve(str(args, "path"))
	pattern := str(args, "pattern")
	var out []string
	more := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == root {
			return nil
		}
		if d.IsDir() && ignoredDirs[d.Name()] {
			return filepath.SkipDir
		}
		if pattern != "" {
			if ok, _ := filepath.Match(pattern, d.Name()); !ok || d.IsDir() {
				return nil
			}
		}
		if len(out) >= 200 {
			more++
			return nil
		}
		r, _ := filepath.Rel(root, p)
		if d.IsDir() {
			r += "/"
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return a.fileError("list_files", root, err)
	}
	if _, serr := os.Stat(root); serr != nil {
		return a.fileError("list_files", root, serr)
	}
	sort.Strings(out)
	s := strings.Join(out, "\n")
	if len(out) == 0 {
		s = "(no files)"
	}
	if more > 0 {
		s += fmt.Sprintf("\n... %d more entries (narrow with path or pattern)", more)
	}
	return ToolResult{Output: s, Status: fmt.Sprintf("%d entri", len(out)+more)}
}

func runWrite(ctx context.Context, a *Agent, args map[string]any) ToolResult {
	path := a.resolve(str(args, "path"))
	content := str(args, "content")
	_, statErr := os.Stat(path)
	existed := statErr == nil
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return a.fileError("write_file", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return a.fileError("write_file", path, err)
	}
	verb, status := "Created", "dibuat"
	if existed {
		verb, status = "Overwrote", "ditimpa"
	}
	n := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		n++
	}
	res := ToolResult{
		Output:  fmt.Sprintf("%s %s (%d lines).", verb, a.rel(path), n),
		Status:  fmt.Sprintf("%s · %d baris", status, n),
		Mutated: path,
	}
	a.applyCheck(ctx, path, &res)
	return res
}

func runEdit(ctx context.Context, a *Agent, args map[string]any) ToolResult {
	path := a.resolve(str(args, "path"))
	oldS, newS := str(args, "old_string"), str(args, "new_string")
	b, err := os.ReadFile(path)
	if err != nil {
		return a.fileError("edit_file", path, err)
	}
	content := string(b)
	if oldS == "" {
		return ToolResult{Invalid: true, ErrKey: "edit_empty", Status: "old_string kosong",
			Output: correction("edit_file: old_string is empty.", "edit_file replaces existing text; it needs the exact text to replace.",
				"Use write_file to create or fully rewrite a file, or give the exact text to replace.", `{"path": "src/a.js", "old_string": "const x = 1;", "new_string": "const x = 2;"}`)}
	}
	if oldS == newS {
		return ToolResult{Invalid: true, ErrKey: "edit_same", Status: "old == new",
			Output: correction("edit_file: old_string and new_string are identical.", "That edit would change nothing.", "Put the changed text in new_string, or skip this edit.", "")}
	}
	count := strings.Count(content, oldS)
	rel := a.rel(path)
	switch {
	case count == 0:
		snippet, why := nearestSnippet(content, oldS)
		return ToolResult{Invalid: true, ErrKey: "edit_nomatch:" + rel, Status: "old_string tidak ditemukan",
			Output: correction("edit_file: old_string was not found in "+rel+".", why,
				"Copy the text exactly from the snippet below (without the line numbers), or read_file again.", "") + "\n" + snippet}
	case count > 1:
		return ToolResult{Invalid: true, ErrKey: "edit_multi:" + rel, Status: fmt.Sprintf("old_string muncul %d kali", count),
			Output: correction(fmt.Sprintf("edit_file: old_string appears %d times in %s (lines %s).", count, rel, matchLines(content, oldS)),
				"The edit must be unambiguous.", "Include one or two surrounding lines in old_string so it matches only once.", "")}
	}
	updated := strings.Replace(content, oldS, newS, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return a.fileError("edit_file", path, err)
	}
	line := strings.Count(content[:strings.Index(content, oldS)], "\n") + 1
	res := ToolResult{
		Output:  fmt.Sprintf("Edited %s at line %d.", rel, line),
		Status:  fmt.Sprintf("baris %d", line),
		Mutated: path,
		Diff:    miniDiff(oldS, newS, 20),
	}
	a.applyCheck(ctx, path, &res)
	return res
}

func matchLines(content, s string) string {
	var ls []string
	off := 0
	for len(ls) < 6 {
		i := strings.Index(content[off:], s)
		if i < 0 {
			break
		}
		ls = append(ls, strconv.Itoa(strings.Count(content[:off+i], "\n")+1))
		off += i + len(s)
	}
	return strings.Join(ls, ", ")
}

// nearestSnippet shows the part of the file most similar to the first line of
// old_string, and explains the likely reason for the mismatch.
func nearestSnippet(content, oldS string) (string, string) {
	lines := strings.Split(content, "\n")
	var first string
	for _, l := range strings.Split(oldS, "\n") {
		if strings.TrimSpace(l) != "" {
			first = strings.TrimSpace(l)
			break
		}
	}
	best, bestScore := -1, 0
	why := "The text must match the file exactly, character for character."
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if t == first {
			best = i
			why = fmt.Sprintf("Line %d has the same text but different indentation/whitespace, or the following lines differ.", i+1)
			break
		}
		if sc := tokenOverlap(t, first); sc > bestScore {
			best, bestScore = i, sc
		}
	}
	if best < 0 {
		return "(no similar lines found; read_file the file first)", why
	}
	from, to := max(best-5, 0), min(best+6, len(lines))
	var sb strings.Builder
	fmt.Fprintf(&sb, "Snippet (lines %d-%d):\n", from+1, to)
	for i := from; i < to; i++ {
		fmt.Fprintf(&sb, "%5d\t%s\n", i+1, lines[i])
	}
	return sb.String(), why
}

var reTokens = regexp.MustCompile(`[A-Za-z0-9_]+`)

func tokenOverlap(a, b string) int {
	set := map[string]bool{}
	for _, t := range reTokens.FindAllString(b, -1) {
		set[t] = true
	}
	n := 0
	for _, t := range reTokens.FindAllString(a, -1) {
		if set[t] {
			n++
			delete(set, t)
		}
	}
	return n
}

func miniDiff(oldS, newS string, maxLines int) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(oldS, "\n"), "\n") {
		out = append(out, "- "+l)
	}
	for _, l := range strings.Split(strings.TrimRight(newS, "\n"), "\n") {
		out = append(out, "+ "+l)
	}
	if len(out) > maxLines {
		n := len(out) - maxLines
		out = append(out[:maxLines], fmt.Sprintf("… (+%d baris)", n))
	}
	return out
}

func runAsk(ctx context.Context, a *Agent, args map[string]any) ToolResult {
	var opts []string
	for _, o := range asSlice(args["options"]) {
		if s := strings.TrimSpace(fmt.Sprint(o)); s != "" {
			opts = append(opts, s)
		}
	}
	if a.askLine == nil {
		return ToolResult{Status: "tidak ada user interaktif",
			Output: "[harness] Nobody can answer: this is a non-interactive run. Pick the safest sensible option yourself, say in your final answer which one you picked and why, and continue."}
	}
	a.ui.Options(opts)
	ans, err := a.askLine(ctx, "  jawab (nomor atau teks bebas) › ")
	if err != nil {
		return ToolResult{Denied: true, Status: "dibatalkan",
			Output: "[harness] The user did not answer and stopped the turn. Wait for their next message."}
	}
	ans = strings.TrimSpace(ans)
	if n, e := strconv.Atoi(ans); e == nil && n >= 1 && n <= len(opts) {
		ans = opts[n-1]
	}
	if ans == "" {
		return ToolResult{Status: "tidak dijawab",
			Output: "[harness] The user pressed enter without answering. Choose the most reasonable option, say which one, and continue."}
	}
	if a.plan != nil { // a decision by the user is a fact about the world: it crosses subtasks
		a.plan.Answers = append(a.plan.Answers, clip(str(args, "question"), 120)+" -> "+clip(ans, 120))
	}
	return ToolResult{Output: "The user answered: " + ans, Status: clip(ans, 60)}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

const todoExample = `{"items": [{"text": "read src/math.js", "status": "done"}, {"text": "fix sum", "status": "in_progress"}, {"text": "run npm test", "status": "pending"}]}`

type Todo struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

var todoStatus = map[string]string{
	"pending": "pending", "todo": "pending", "not_started": "pending", "open": "pending",
	"in_progress": "in_progress", "in-progress": "in_progress", "inprogress": "in_progress", "doing": "in_progress", "active": "in_progress",
	"done": "done", "completed": "done", "complete": "done", "finished": "done",
}

func runTodo(ctx context.Context, a *Agent, args map[string]any) ToolResult {
	raw := asSlice(args["items"])
	var todos []Todo
	for i, it := range raw {
		var t Todo
		switch x := it.(type) {
		case string:
			t = Todo{Text: x, Status: "pending"}
		case map[string]any:
			for _, k := range []string{"text", "content", "task", "title", "step", "description"} {
				if s, ok := x[k].(string); ok && s != "" {
					t.Text = s
					break
				}
			}
			s, _ := x["status"].(string)
			t.Status = todoStatus[strings.ToLower(strings.TrimSpace(s))]
			if t.Status == "" {
				t.Status = "pending"
			}
		}
		if t.Text == "" {
			return ToolResult{Invalid: true, ErrKey: "todo_item", Status: "item todo tidak valid",
				Output: correction(fmt.Sprintf("todo: item %d has no text.", i+1), "Each item needs a text and a status.", "Send the full list again.", todoExample)}
		}
		todos = append(todos, t)
	}
	todos, note := a.auditTodos(todos)
	a.todos = todos
	a.todoMutations = a.mutations
	res := ToolResult{Output: "Plan updated.\n" + todoList(todos), Status: todoCounts(todos)}
	if note != "" {
		res.Output += "\n\n" + note
		res.Status += " · centang ditolak"
	}
	return res
}

// auditTodos keeps the checkmarks honest. The model owns the list; the harness
// owns "done": an item may only become done when the work since the previous
// plan update was verified, or when nothing was changed at all (a reading
// step). A list that shrinks is announced, because a plan rewritten to fit
// what got done is exactly the failure this exists to catch.
func (a *Agent) auditTodos(next []Todo) ([]Todo, string) {
	prevDone, prevOpen := map[string]bool{}, map[string]bool{}
	for _, t := range a.todos {
		if t.Status == "done" {
			prevDone[t.Text] = true
		} else {
			prevOpen[t.Text] = true
		}
	}
	mutatedSince := a.mutations > a.todoMutations
	verified := a.turn != nil && a.turn.verified()
	var demoted, milestone []string
	seen := map[string]bool{}
	for i, t := range next {
		seen[t.Text] = true
		if t.Status == "done" && !prevDone[t.Text] && mutatedSince {
			if verified {
				milestone = append(milestone, t.Text)
			} else {
				next[i].Status = "in_progress"
				demoted = append(demoted, t.Text)
			}
		}
	}
	// A step closed with verified changes is a milestone: the harness will
	// checkpoint the context after this batch. Reading steps are not one -
	// compacting would throw away exactly what was just read.
	if a.turn != nil && len(milestone) > 0 {
		a.turn.milestone = milestone
	}
	removed := 0
	for text := range prevOpen {
		if !seen[text] {
			removed++
		}
	}
	if removed > 0 {
		a.ui.Info(fmt.Sprintf("rencana diubah: %d langkah dihapus", removed))
	}
	if len(demoted) == 0 {
		return next, ""
	}
	a.ui.Harness(fmt.Sprintf("%d centang todo ditolak: perubahan belum diverifikasi", len(demoted)))
	how := "run the project's tests or build"
	if len(a.env.VerifyCmds) > 0 {
		how = "run `" + a.env.VerifyCmds[0] + "`"
	}
	return next, correction(
		"These steps were not accepted as done: "+strings.Join(demoted, "; ")+".",
		"Files changed since the plan was last updated and nothing verified that change. The harness keeps the checkmarks; a step is done when its result is proven, not when it is declared.",
		"Verify first ("+how+"), then send the plan again with the step marked done.", "")
}

func todoList(ts []Todo) string {
	var sb strings.Builder
	for _, t := range ts {
		mark := map[string]string{"pending": "[ ]", "in_progress": "[>]", "done": "[x]"}[t.Status]
		fmt.Fprintf(&sb, "%s %s\n", mark, t.Text)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func todoCounts(ts []Todo) string {
	done := 0
	for _, t := range ts {
		if t.Status == "done" {
			done++
		}
	}
	return fmt.Sprintf("%d/%d selesai", done, len(ts))
}

func openTodos(ts []Todo) []string {
	var out []string
	for _, t := range ts {
		if t.Status != "done" {
			out = append(out, t.Text)
		}
	}
	return out
}
