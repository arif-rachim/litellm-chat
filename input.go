package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
	"unicode"
	"unsafe"
)

// A small raw-mode line editor. It is what makes Tab a key the program can see
// (in cooked mode the terminal only hands over whole lines), and it also gives
// history and single-key confirmations. Raw mode is entered only while reading,
// so Ctrl+C keeps working as SIGINT everywhere else.

type termState struct {
	fd  uintptr
	old syscall.Termios
}

func rawMode(f *os.File) *termState {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil
	}
	old := t
	t.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG | syscall.IEXTEN
	t.Cc[syscall.VMIN], t.Cc[syscall.VTIME] = 1, 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil
	}
	return &termState{f.Fd(), old}
}

func (s *termState) restore() {
	if s != nil {
		syscall.Syscall(syscall.SYS_IOCTL, s.fd, syscall.TCSETS, uintptr(unsafe.Pointer(&s.old)))
	}
}

type Editor struct {
	in      *bufio.Reader
	file    *os.File // nil: no terminal to switch (tests)
	out     io.Writer
	history []string
	onTab   func()              // Tab: switch mode
	onClip  func() string       // Ctrl+V: returns text to insert (images are attached by the caller)
	onPaste func(string) string // bracketed paste: returns text to insert

	cmds    []Completion // slash commands offered while typing and on Tab
	color   bool         // ANSI colors in the suggestion list
	cols    int          // terminal width override (tests)
	lastRow int          // cursor row inside the block drawn by the last render
}

// Completion is one slash command the editor can suggest and complete.
type Completion struct{ Name, Args, Help string }

func NewEditor(f *os.File, out io.Writer) *Editor {
	return &Editor{in: bufio.NewReader(f), file: f, out: out}
}

// readKey returns a key name, and the rune itself for "char".
func (e *Editor) readKey() (string, rune, error) {
	r, _, err := e.in.ReadRune()
	if err != nil {
		return "", 0, err
	}
	switch r {
	case '\r', '\n':
		return "enter", 0, nil
	case 127, 8:
		return "back", 0, nil
	case '\t':
		return "tab", 0, nil
	case 3:
		return "ctrl-c", 0, nil
	case 4:
		return "ctrl-d", 0, nil
	case 1:
		return "home", 0, nil
	case 5:
		return "end", 0, nil
	case 21:
		return "kill", 0, nil
	case 23:
		return "wordkill", 0, nil
	case 22: // Ctrl+V
		return "clip", 0, nil
	case 27:
		if e.in.Buffered() == 0 {
			return "esc", 0, nil
		}
		b, _, _ := e.in.ReadRune()
		if b != '[' && b != 'O' {
			return "esc", 0, nil
		}
		var seq []rune
		for {
			c, _, err := e.in.ReadRune()
			if err != nil {
				return "esc", 0, err
			}
			if c >= 0x40 && c <= 0x7e { // final byte
				seq = append(seq, c)
				break
			}
			seq = append(seq, c)
		}
		switch string(seq) {
		case "A":
			return "up", 0, nil
		case "B":
			return "down", 0, nil
		case "C":
			return "right", 0, nil
		case "D":
			return "left", 0, nil
		case "H", "1~":
			return "home", 0, nil
		case "F", "4~":
			return "end", 0, nil
		case "3~":
			return "del", 0, nil
		case "200~":
			return "paste", 0, nil
		}
		return "esc", 0, nil
	}
	if r < 32 {
		return "esc", 0, nil
	}
	return "char", r, nil
}

// readPasted collects a bracketed paste up to the closing marker.
func (e *Editor) readPasted() string {
	var b strings.Builder
	for {
		r, _, err := e.in.ReadRune()
		if err != nil {
			return b.String()
		}
		if r == 27 {
			rest, _ := e.in.Peek(5)
			if strings.HasPrefix(string(rest), "[201~") {
				e.in.Discard(5)
				return b.String()
			}
		}
		b.WriteRune(r)
	}
}

// matches returns the commands a half-typed command word could become. Only a
// first word starting with "/" is one: once there is a space the line is an
// argument, not a command name.
func (e *Editor) matches(line string) []Completion {
	if len(e.cmds) == 0 || !strings.HasPrefix(line, "/") || strings.ContainsAny(line, " \t") {
		return nil
	}
	var out []Completion
	for _, c := range e.cmds {
		if strings.HasPrefix(c.Name, line) {
			out = append(out, c)
		}
	}
	return out
}

// complete expands a half-typed command to the longest name all the candidates
// share. It reports false when the line is not a command word, which is how Tab
// keeps its other job: switching mode.
func (e *Editor) complete(line string) (string, bool) {
	m := e.matches(line)
	switch len(m) {
	case 0:
		return line, false
	case 1:
		if m[0].Args != "" {
			return m[0].Name + " ", true
		}
		return m[0].Name, true
	}
	common := m[0].Name
	for _, c := range m[1:] {
		for !strings.HasPrefix(c.Name, common) {
			common = common[:len(common)-1]
		}
	}
	return common, true // already at the common prefix: the list below says why
}

const menuMax = 8 // suggestion rows; the rest is a "… n lagi" line

// menu is the suggestion list drawn under the line being typed. Rows are kept
// inside the terminal width: a row that wrapped would break the row counting
// render relies on.
func (e *Editor) menu(line string, w int) []string {
	m := e.matches(line)
	if len(m) == 0 {
		return nil
	}
	paint := func(code, s string) string {
		if !e.color {
			return s
		}
		return code + s + sgrReset
	}
	var rows []string
	for i, c := range m {
		if len(m) > menuMax && i == menuMax-1 {
			rows = append(rows, paint(sgrDim, fmt.Sprintf("  … %d lagi", len(m)-i)))
			break
		}
		name := c.Name
		if c.Args != "" {
			name += " " + c.Args
		}
		name = fmt.Sprintf("%-17s", name)
		row := "  " + paint(sgrCyan, name)
		if room := w - 1 - 2 - len([]rune(name)) - 1; room > 0 {
			row += " " + paint(sgrDim, clip(c.Help, room))
		}
		rows = append(rows, row)
	}
	return rows
}

type winsize struct{ row, col, xpixel, ypixel uint16 }

// width is how many columns the terminal has. Without a terminal (tests) the
// classic 80 is assumed.
func (e *Editor) width() int {
	if e.cols > 0 {
		return e.cols
	}
	if e.file == nil {
		return 80
	}
	var ws winsize
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, e.file.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); err != 0 || ws.col == 0 {
		return 80
	}
	return int(ws.col)
}

// Column widths. A terminal gives a CJK ideograph or an emoji two cells, a
// combining mark none at all, so the caret arithmetic below cannot count runes.

// wideRanges are the East Asian Wide/Fullwidth and emoji code points, the ones
// a terminal draws two cells wide.
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x2329, 0x232A}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0},
	{0x23F3, 0x23F3}, {0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653},
	{0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1}, {0x26AA, 0x26AB},
	{0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE}, {0x26D4, 0x26D4},
	{0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA},
	{0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728},
	{0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757},
	{0x2795, 0x2797}, {0x27B0, 0x27B0}, {0x27BF, 0x27BF}, {0x2B1B, 0x2B1C},
	{0x2B50, 0x2B50}, {0x2B55, 0x2B55}, {0x2E80, 0x303E}, {0x3041, 0x33FF},
	{0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0xA000, 0xA4CF}, {0xA960, 0xA97F},
	{0xAC00, 0xD7A3}, {0xF900, 0xFAFF}, {0xFE10, 0xFE19}, {0xFE30, 0xFE6F},
	{0xFF00, 0xFF60}, {0xFFE0, 0xFFE6}, {0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF},
	{0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A}, {0x1F1E6, 0x1F1FF}, {0x1F200, 0x1F320},
	{0x1F32D, 0x1F335}, {0x1F337, 0x1F37C}, {0x1F37E, 0x1F393}, {0x1F3A0, 0x1F3CA},
	{0x1F3CF, 0x1F3D3}, {0x1F3E0, 0x1F3F0}, {0x1F3F4, 0x1F3F4}, {0x1F3F8, 0x1F43E},
	{0x1F440, 0x1F440}, {0x1F442, 0x1F4FC}, {0x1F4FF, 0x1F53D}, {0x1F54B, 0x1F54E},
	{0x1F550, 0x1F567}, {0x1F57A, 0x1F57A}, {0x1F595, 0x1F596}, {0x1F5A4, 0x1F5A4},
	{0x1F5FB, 0x1F64F}, {0x1F680, 0x1F6C5}, {0x1F6CC, 0x1F6CC}, {0x1F6D0, 0x1F6D2},
	{0x1F6D5, 0x1F6D7}, {0x1F6EB, 0x1F6EC}, {0x1F6F4, 0x1F6FC}, {0x1F7E0, 0x1F7EB},
	{0x1F90C, 0x1F93A}, {0x1F93C, 0x1F945}, {0x1F947, 0x1F9FF}, {0x1FA70, 0x1FAFF},
	{0x20000, 0x2FFFD}, {0x30000, 0x3FFFD},
}

// runeWidth is how many cells a rune takes. Terminals disagree about ZWJ emoji
// sequences and flag pairs, so those can still be off by a cell; everything a
// coding session actually types is right.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || r == 0x7f: // control
		return 0
	case r == 0x200B || r == 0x200D || r == 0x2060 || r == 0xFEFF:
		return 0
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r): // combining, selectors
		return 0
	}
	for _, rg := range wideRanges {
		if r >= rg[0] && r <= rg[1] {
			return 2
		}
		if r < rg[0] {
			break // the table is sorted
		}
	}
	return 1
}

// advance walks rs from cell (row, col) and reports where the cursor lands, the
// way the terminal moves it: a rune that no longer fits wraps to the next row,
// and a column equal to the width means "parked at the edge, wrap pending".
func advance(row, col, w int, rs []rune) (int, int) {
	for i, r := range rs {
		cw := runeWidth(r)
		if cw == 1 && i+1 < len(rs) && rs[i+1] == 0xFE0F {
			cw = 2 // the emoji selector turns the character into a wide one
		}
		if cw == 0 {
			continue
		}
		if col+cw > w {
			row, col = row+1, 0
		}
		col += cw
	}
	return row, col
}

// promptRunes is the prompt without its ANSI color escapes.
func promptRunes(s string) []rune {
	var out []rune
	esc := false
	for _, r := range s {
		switch {
		case esc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				esc = false
			}
		case r == 27:
			esc = true
		default:
			out = append(out, r)
		}
	}
	return out
}

// render redraws the prompt, the line and the suggestions. A typed line is
// regularly longer than the terminal is wide, so the drawing is counted in
// rows: go back to the first row of what was drawn last time, wipe everything
// below, draw again, then put the cursor where the caret belongs.
func (e *Editor) render(prompt string, buf []rune, cur int) {
	w := e.width()
	if e.lastRow > 0 {
		fmt.Fprintf(e.out, "\x1b[%dA", e.lastRow)
	}
	fmt.Fprint(e.out, "\r\x1b[J")

	pr, pc := advance(0, 0, w, promptRunes(prompt))
	fmt.Fprintf(e.out, "%s%s", prompt, string(buf))
	rows, endCol := advance(pr, pc, w, buf)
	if endCol >= w {
		fmt.Fprint(e.out, "\n") // sit on the next row instead of a pending wrap
		rows++
	}
	for _, r := range e.menu(string(buf), w) {
		fmt.Fprintf(e.out, "\n%s", r)
		rows++
	}

	curRow, curCol := advance(pr, pc, w, buf[:cur])
	if curCol >= w {
		curRow, curCol = curRow+1, 0
	}
	if up := rows - curRow; up > 0 {
		fmt.Fprintf(e.out, "\x1b[%dA", up)
	}
	fmt.Fprint(e.out, "\r")
	if curCol > 0 {
		fmt.Fprintf(e.out, "\x1b[%dC", curCol)
	}
	e.lastRow = curRow
}

// endLine drops the suggestions, leaves the finished line on screen and moves
// to a fresh row. Every way out of ReadLine goes through it.
func (e *Editor) endLine(prompt string, buf []rune) {
	if e.lastRow > 0 {
		fmt.Fprintf(e.out, "\x1b[%dA", e.lastRow)
	}
	fmt.Fprintf(e.out, "\r\x1b[J%s%s\n", prompt, string(buf))
	e.lastRow = 0
}

// ReadLine reads one line. prompt is a function so the prompt can change while
// the line is being typed (Tab switches the mode shown in it).
func (e *Editor) ReadLine(prompt func() string) (string, error) {
	defer rawMode(e.file).restore()
	fmt.Fprint(e.out, "\x1b[?2004h")
	defer fmt.Fprint(e.out, "\x1b[?2004l")
	var buf []rune
	cur, hist := 0, len(e.history)
	e.lastRow = 0
	e.render(prompt(), buf, cur)
	for {
		key, r, err := e.readKey()
		if err != nil {
			e.endLine(prompt(), buf)
			return "", err
		}
		switch key {
		case "enter":
			e.endLine(prompt(), buf)
			line := string(buf)
			if strings.TrimSpace(line) != "" {
				e.history = append(e.history, line)
			}
			return line, nil
		case "ctrl-c":
			e.endLine(prompt(), buf)
			return "", errInterrupt
		case "ctrl-d":
			if len(buf) == 0 {
				e.endLine(prompt(), buf)
				return "", io.EOF
			}
		case "tab":
			if line, ok := e.complete(string(buf)); ok {
				buf, cur = []rune(line), len([]rune(line))
			} else if e.onTab != nil {
				e.onTab()
			}
		case "clip":
			if e.onClip != nil {
				for _, r := range e.onClip() {
					buf = slices.Insert(buf, cur, r)
					cur++
				}
			}
		case "paste":
			text := e.readPasted()
			if e.onPaste != nil {
				text = e.onPaste(text)
			}
			for _, r := range strings.ReplaceAll(text, "\n", " ") {
				buf = slices.Insert(buf, cur, r)
				cur++
			}
		case "char":
			buf = slices.Insert(buf, cur, r)
			cur++
		case "back":
			if cur > 0 {
				buf = slices.Delete(buf, cur-1, cur)
				cur--
			}
		case "del":
			if cur < len(buf) {
				buf = slices.Delete(buf, cur, cur+1)
			}
		case "left":
			if cur > 0 {
				cur--
			}
		case "right":
			if cur < len(buf) {
				cur++
			}
		case "home":
			cur = 0
		case "end":
			cur = len(buf)
		case "kill":
			buf, cur = nil, 0
		case "wordkill":
			i := cur
			for i > 0 && buf[i-1] == ' ' {
				i--
			}
			for i > 0 && buf[i-1] != ' ' {
				i--
			}
			buf = slices.Delete(buf, i, cur)
			cur = i
		case "up":
			if hist > 0 {
				hist--
				buf, cur = []rune(e.history[hist]), len([]rune(e.history[hist]))
			}
		case "down":
			switch {
			case hist < len(e.history)-1:
				hist++
				buf, cur = []rune(e.history[hist]), len([]rune(e.history[hist]))
			default:
				hist = len(e.history)
				buf, cur = nil, 0
			}
		}
		e.render(prompt(), buf, cur)
	}
}

// ReadSecret reads a line without echoing it (API keys). A long key wraps just
// like a typed line, so it is drawn with the same row-aware renderer.
func (e *Editor) ReadSecret(prompt string) (string, error) {
	defer rawMode(e.file).restore()
	var buf []rune
	e.lastRow = 0
	mask := func() []rune { return []rune(strings.Repeat("•", len(buf))) }
	e.render(prompt, nil, 0)
	for {
		key, r, err := e.readKey()
		if err != nil {
			e.endLine(prompt, mask())
			return "", err
		}
		switch key {
		case "enter":
			e.endLine(prompt, mask())
			return string(buf), nil
		case "ctrl-c":
			e.endLine(prompt, mask())
			return "", errInterrupt
		case "char":
			buf = append(buf, r)
		case "back":
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
			}
		case "kill":
			buf = nil
		case "clip":
			if e.onClip != nil {
				buf = append(buf, []rune(e.onClip())...)
			}
		case "paste":
			buf = append(buf, []rune(strings.TrimSpace(e.readPasted()))...)
		}
		m := mask()
		e.render(prompt, m, len(m))
	}
}

// ReadChoice waits for one of keys (or Enter for the default) without Enter.
func (e *Editor) ReadChoice(prompt, keys string) (string, error) {
	defer rawMode(e.file).restore()
	fmt.Fprintf(e.out, "\r\x1b[K%s", prompt)
	for {
		key, r, err := e.readKey()
		if err != nil {
			fmt.Fprint(e.out, "\n")
			return "", err
		}
		switch key {
		case "enter":
			fmt.Fprint(e.out, "\n")
			return "", nil
		case "ctrl-c", "ctrl-d":
			fmt.Fprint(e.out, "\n")
			return "", errInterrupt
		case "char":
			c := strings.ToLower(string(r))
			if strings.Contains(keys, c) {
				fmt.Fprintf(e.out, "%s\n", c)
				return c, nil
			}
		}
	}
}
