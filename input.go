package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
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

	cmds     []Completion // slash commands offered while typing and on Tab
	color    bool         // ANSI colors in the suggestion list
	menuRows int          // suggestion rows currently drawn below the line
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

// menu is the suggestion list drawn under the line being typed.
func (e *Editor) menu(line string) []string {
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
		rows = append(rows, "  "+paint(sgrCyan, fmt.Sprintf("%-17s", name))+" "+paint(sgrDim, c.Help))
	}
	return rows
}

// clearMenu erases the suggestion rows and leaves the cursor on the line again.
func (e *Editor) clearMenu() {
	if e.menuRows == 0 {
		return
	}
	for range e.menuRows {
		fmt.Fprint(e.out, "\n\x1b[K")
	}
	fmt.Fprintf(e.out, "\x1b[%dA", e.menuRows)
	e.menuRows = 0
}

func (e *Editor) render(prompt string, buf []rune, cur int) {
	e.clearMenu()
	fmt.Fprintf(e.out, "\r\x1b[K%s%s", prompt, string(buf))
	if rows := e.menu(string(buf)); len(rows) > 0 {
		for _, r := range rows {
			fmt.Fprintf(e.out, "\n\x1b[K%s", r)
		}
		// Back up to the line and redraw it: that is cheaper to get right than
		// counting the printable columns of a prompt full of escape codes.
		fmt.Fprintf(e.out, "\x1b[%dA\r%s%s", len(rows), prompt, string(buf))
		e.menuRows = len(rows)
	}
	if d := len(buf) - cur; d > 0 {
		fmt.Fprintf(e.out, "\x1b[%dD", d)
	}
}

// ReadLine reads one line. prompt is a function so the prompt can change while
// the line is being typed (Tab switches the mode shown in it).
func (e *Editor) ReadLine(prompt func() string) (string, error) {
	defer rawMode(e.file).restore()
	fmt.Fprint(e.out, "\x1b[?2004h")
	defer fmt.Fprint(e.out, "\x1b[?2004l")
	var buf []rune
	cur, hist := 0, len(e.history)
	e.render(prompt(), buf, cur)
	for {
		key, r, err := e.readKey()
		if err != nil {
			e.clearMenu()
			fmt.Fprint(e.out, "\n")
			return "", err
		}
		switch key {
		case "enter":
			e.clearMenu()
			fmt.Fprint(e.out, "\n")
			line := string(buf)
			if strings.TrimSpace(line) != "" {
				e.history = append(e.history, line)
			}
			return line, nil
		case "ctrl-c":
			e.clearMenu()
			fmt.Fprint(e.out, "\n")
			return "", errInterrupt
		case "ctrl-d":
			if len(buf) == 0 {
				e.clearMenu()
				fmt.Fprint(e.out, "\n")
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

// ReadSecret reads a line without echoing it (API keys).
func (e *Editor) ReadSecret(prompt string) (string, error) {
	defer rawMode(e.file).restore()
	var buf []rune
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
			return string(buf), nil
		case "ctrl-c":
			fmt.Fprint(e.out, "\n")
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
		fmt.Fprintf(e.out, "\r\x1b[K%s%s", prompt, strings.Repeat("•", len(buf)))
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
