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
	if rows := e.menu("/c", 80); len(rows) != 2 || !strings.Contains(rows[0], "/clear") {
		t.Fatalf("suggestions for /c: %q", rows)
	}
	if rows := e.menu("/model q", 80); rows != nil {
		t.Fatalf("an argument is not a command word: %q", rows)
	}
	// Past the command word Tab is the mode switch again.
	mode = "ask"
	if line, _, _ := testEditorCmds("/model q\t\r", func() { mode = "auto" }, cmds); line != "/model q" || mode != "auto" {
		t.Fatalf("tab in an argument: line=%q mode=%q", line, mode)
	}
	// The suggestion rows are wiped before the line is handed over.
	if _, out, _ := testEditorCmds("/c\r", nil, cmds); !strings.HasSuffix(strings.TrimSuffix(out, "\x1b[?2004l"), "\r\x1b[J› /c\n") {
		t.Fatalf("menu not cleared on enter: %q", out)
	}
}

// tinyTerm applies the escape sequences the editor writes to a character grid,
// so a test can assert on what the user would actually see.
type tinyTerm struct {
	w       int
	rows    [][]rune
	r, c    int
	pending bool // cursor parked in the last column, waiting to wrap
}

func newTinyTerm(w int) *tinyTerm { return &tinyTerm{w: w, rows: [][]rune{{}}} }

func (t *tinyTerm) row(i int) []rune {
	for len(t.rows) <= i {
		t.rows = append(t.rows, []rune{})
	}
	return t.rows[i]
}

// put writes one rune, giving a wide rune the two cells a terminal gives it.
// The second cell holds filler that screen() drops again.
func (t *tinyTerm) put(ch rune) {
	cw := runeWidth(ch)
	if cw == 0 {
		return
	}
	if t.pending {
		t.r, t.c, t.pending = t.r+1, 0, false
	}
	if t.c+cw > t.w {
		t.r, t.c = t.r+1, 0
	}
	row := t.row(t.r)
	for len(row) < t.c+cw {
		row = append(row, ' ')
	}
	row[t.c] = ch
	if cw == 2 {
		row[t.c+1] = filler
	}
	t.rows[t.r] = row
	if t.c += cw; t.c >= t.w {
		t.pending = true
	}
}

// filler marks the second cell of a wide rune.
const filler = '\x00'

func (t *tinyTerm) write(s string) {
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch {
		case rs[i] == '\r':
			t.c, t.pending = 0, false
		case rs[i] == '\n':
			t.r, t.pending = t.r+1, false
			t.row(t.r)
		case rs[i] == 27 && i+1 < len(rs) && rs[i+1] == '[':
			j, num, private := i+2, 0, false
			if j < len(rs) && rs[j] == '?' { // private mode, e.g. bracketed paste
				j, private = j+1, true
			}
			for ; j < len(rs) && rs[j] >= '0' && rs[j] <= '9'; j++ {
				num = num*10 + int(rs[j]-'0')
			}
			if j >= len(rs) {
				return
			}
			if num == 0 {
				num = 1
			}
			if private { // not a cursor movement: nothing to draw
				i = j
				continue
			}
			switch rs[j] {
			case 'A':
				t.r, t.pending = max(t.r-num, 0), false
			case 'B':
				t.r, t.pending = t.r+num, false
			case 'C':
				t.c, t.pending = t.c+num, false
			case 'D':
				t.c, t.pending = max(t.c-num, 0), false
			case 'K': // erase to end of line
				if row := t.row(t.r); len(row) > t.c {
					t.rows[t.r] = row[:t.c]
				}
			case 'J': // erase to end of screen
				if row := t.row(t.r); len(row) > t.c {
					t.rows[t.r] = row[:t.c]
				}
				t.rows = t.rows[:t.r+1]
			}
			i = j
		case rs[i] == 0xFE0F: // already counted with the rune before it
		default:
			t.put(rs[i])
		}
	}
}

// screen is what is on the grid, rows joined as the terminal would wrap them.
func (t *tinyTerm) screen() string {
	var b strings.Builder
	for _, row := range t.rows {
		b.WriteString(strings.TrimRight(strings.ReplaceAll(string(row), string(filler), ""), " "))
	}
	return b.String()
}

func TestLineEditorLongLineDoesNotRepeat(t *testing.T) {
	const w = 40
	long := strings.Repeat("panjang ", 12) + "selesai" // jauh lebih lebar dari 40 kolom
	var out bytes.Buffer
	e := &Editor{in: bufio.NewReader(strings.NewReader(long + "\r")), out: &out, cols: w}
	line, err := e.ReadLine(func() string { return "› " })
	if line != long || err != nil {
		t.Fatalf("line: %q %v", line, err)
	}
	scr := newTinyTerm(w)
	scr.write(out.String())
	if got, want := scr.screen(), "› "+long; got != want {
		t.Fatalf("layar menampilkan teks berulang / rusak:\n got: %q\nwant: %q", got, want)
	}
	// The whole thing must fit in the rows it actually needs, not one per keystroke.
	if rows := len(scr.rows); rows > (len([]rune(long))+2)/w+2 {
		t.Fatalf("terlalu banyak baris terpakai: %d", rows)
	}
}

func TestLineEditorEditsWrappedLine(t *testing.T) {
	const w = 30
	// Type past the wrap, then go back and fix a character on the first row.
	keys := strings.Repeat("x", 45) + "\x1b[D\x1b[D\x1b[D" + "Z" + "\r"
	var out bytes.Buffer
	e := &Editor{in: bufio.NewReader(strings.NewReader(keys)), out: &out, cols: w}
	line, _ := e.ReadLine(func() string { return "› " })
	want := strings.Repeat("x", 42) + "Z" + "xxx"
	if line != want {
		t.Fatalf("editing a wrapped line: %q", line)
	}
	scr := newTinyTerm(w)
	scr.write(out.String())
	if got := scr.screen(); got != "› "+want {
		t.Fatalf("layar: %q", got)
	}
}

func TestRuneWidth(t *testing.T) {
	cases := []struct {
		r rune
		w int
	}{
		{'a', 1}, {'›', 1}, {'世', 2}, {'界', 2}, {'こ', 2}, {'한', 2},
		{'😀', 2}, {'✅', 2}, {'\u0301', 0}, {'\uFE0F', 0}, {'\u200D', 0},
	}
	for _, c := range cases {
		if got := runeWidth(c.r); got != c.w {
			t.Errorf("runeWidth(%q) = %d, mau %d", c.r, got, c.w)
		}
	}
	// advance() short-circuits on the sorted table, so it has to stay sorted.
	for i := 1; i < len(wideRanges); i++ {
		if wideRanges[i][0] <= wideRanges[i-1][1] {
			t.Fatalf("wideRanges tidak urut di indeks %d: %v setelah %v", i, wideRanges[i], wideRanges[i-1])
		}
	}
}

func TestLineEditorWideRunes(t *testing.T) {
	const w = 20
	text := "halo 世界 こんにちは 😀 ok"
	var out bytes.Buffer
	e := &Editor{in: bufio.NewReader(strings.NewReader(text + "\r")), out: &out, cols: w}
	line, err := e.ReadLine(func() string { return "› " })
	if line != text || err != nil {
		t.Fatalf("line: %q %v", line, err)
	}
	scr := newTinyTerm(w)
	scr.write(out.String())
	if got, want := scr.screen(), "› "+text; got != want {
		t.Fatalf("layar dengan aksara lebar:\n got: %q\nwant: %q", got, want)
	}
}

func TestLineEditorCaretAfterWideRunes(t *testing.T) {
	const w = 24
	// Type CJK, walk the caret back over two of them, and insert there.
	keys := "世界中国語\x1b[D\x1b[D" + "X" + "\r"
	var out bytes.Buffer
	e := &Editor{in: bufio.NewReader(strings.NewReader(keys)), out: &out, cols: w}
	line, _ := e.ReadLine(func() string { return "› " })
	if want := "世界中X国語"; line != want {
		t.Fatalf("menyisip di tengah aksara lebar: %q, mau %q", line, want)
	}
	// The caret must be reported two cells per CJK rune, not one.
	if row, col := advance(0, 2, w, []rune("世界中")); row != 0 || col != 8 {
		t.Fatalf("advance atas aksara lebar: row=%d col=%d, mau row=0 col=8", row, col)
	}
	scr := newTinyTerm(w)
	scr.write(out.String())
	if got := scr.screen(); got != "› 世界中X国語" {
		t.Fatalf("layar: %q", got)
	}
}
