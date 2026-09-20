package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	sgrReset   = "\x1b[0m"
	sgrBold    = "\x1b[1m"
	sgrDim     = "\x1b[2m"
	sgrItalic  = "\x1b[3m"
	sgrRed     = "\x1b[31m"
	sgrGreen   = "\x1b[32m"
	sgrYellow  = "\x1b[33m"
	sgrBlue    = "\x1b[34m"
	sgrCyan    = "\x1b[36m"
	sgrHeading = "\x1b[1;34m"
)

// isTTY reports whether f is a terminal. A character-device check is not
// enough: /dev/null is one too.
func isTTY(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// ---------------------------------------------------------------------------
// Streaming markdown renderer. It keeps only a few pending characters so text
// still appears token by token, and renders the same output however the input
// is split.

type Markdown struct {
	w         io.Writer
	color     bool
	pend      string
	lineStart bool
	inFence   bool
	heading   bool
	bold      bool
	code      bool
}

func NewMarkdown(w io.Writer, color bool) *Markdown {
	return &Markdown{w: w, color: color, lineStart: true}
}

func (m *Markdown) Write(s string) {
	if !m.color {
		io.WriteString(m.w, s)
		return
	}
	m.pend += s
	m.process(false)
}

// Flush renders anything pending and resets state for the next response.
func (m *Markdown) Flush() {
	if !m.color {
		return
	}
	m.process(true)
	if m.bold || m.code || m.heading {
		io.WriteString(m.w, sgrReset)
	}
	*m = Markdown{w: m.w, color: m.color, lineStart: true}
}

func (m *Markdown) style() string {
	s := sgrReset
	if m.heading {
		s += sgrHeading
	}
	if m.bold {
		s += sgrBold
	}
	if m.code {
		s += sgrCyan
	}
	return s
}

// needMore reports whether a line prefix could still turn into a line-level
// construct (heading, bullet, fence) once more characters arrive.
func needMore(t string) bool {
	if t == "" || t == "-" || t == "*" || t == "+" {
		return true
	}
	if len(t) < 3 && strings.HasPrefix("```", t) {
		return true
	}
	return len(t) <= 6 && strings.Trim(t, "#") == ""
}

func (m *Markdown) process(final bool) {
	var b strings.Builder
	dim := func(s string) string { return sgrDim + s + sgrReset }
	consumeLine := func(nl int) {
		if nl < 0 {
			m.pend = ""
		} else {
			m.pend = m.pend[nl+1:]
		}
	}
loop:
	for len(m.pend) > 0 {
		if m.lineStart {
			nl := strings.IndexByte(m.pend, '\n')
			line := m.pend
			if nl >= 0 {
				line = m.pend[:nl]
			}
			t := strings.TrimLeft(line, " \t")
			indent := line[:len(line)-len(t)]
			if m.inFence {
				if strings.HasPrefix(t, "```") {
					if nl < 0 && !final {
						break loop
					}
					m.inFence = false
					b.WriteString(dim("└") + "\n")
					consumeLine(nl)
					continue
				}
				if nl < 0 && !final && (t == "" || (len(t) < 3 && strings.HasPrefix("```", t))) {
					break loop
				}
				b.WriteString(dim("│ "))
				m.lineStart = false
				continue
			}
			if nl < 0 && !final && needMore(t) {
				break loop
			}
			switch {
			case strings.HasPrefix(t, "```"):
				if nl < 0 && !final {
					break loop
				}
				b.WriteString(dim("┌ "+strings.TrimSpace(t[3:])) + "\n")
				consumeLine(nl)
				m.inFence = true
				continue
			case strings.HasPrefix(t, "#"):
				n := len(t) - len(strings.TrimLeft(t, "#"))
				if n <= 6 && len(t) > n && t[n] == ' ' {
					m.pend = m.pend[len(indent)+n+1:]
					m.heading = true
					b.WriteString(m.style())
				}
			case len(t) >= 2 && strings.ContainsRune("-*+", rune(t[0])) && t[1] == ' ':
				m.pend = m.pend[len(indent)+2:]
				b.WriteString(indent + "• ")
			}
			m.lineStart = false
			continue
		}
		if m.inFence {
			i := strings.IndexByte(m.pend, '\n')
			if i < 0 {
				b.WriteString(m.pend)
				m.pend = ""
				break
			}
			b.WriteString(m.pend[:i+1])
			m.pend = m.pend[i+1:]
			m.lineStart = true
			continue
		}
		switch c := m.pend[0]; c {
		case '\n':
			if m.heading || m.bold || m.code {
				m.heading, m.bold, m.code = false, false, false
				b.WriteString(sgrReset)
			}
			b.WriteByte('\n')
			m.pend = m.pend[1:]
			m.lineStart = true
		case '`':
			m.code = !m.code
			b.WriteString(m.style())
			m.pend = m.pend[1:]
		case '*':
			if len(m.pend) < 2 && !final {
				break loop
			}
			if len(m.pend) >= 2 && m.pend[1] == '*' && !m.code {
				m.bold = !m.bold
				b.WriteString(m.style())
				m.pend = m.pend[2:]
			} else {
				b.WriteByte('*')
				m.pend = m.pend[1:]
			}
		default:
			i := strings.IndexAny(m.pend, "\n`*")
			if i < 0 {
				i = len(m.pend)
			}
			b.WriteString(m.pend[:i])
			m.pend = m.pend[i:]
		}
	}
	io.WriteString(m.w, b.String())
}

// ---------------------------------------------------------------------------
// UI: everything the user sees. Assistant text goes to out (stdout); activity
// (tools, reasoning, harness notes) goes to log, which is stderr in -p mode so
// pipes only get the answer.

type UI struct {
	mu         sync.Mutex
	out, log   io.Writer
	md         *Markdown
	color      bool // ANSI on the log stream
	verbose    bool
	quietThink bool
	col0       bool
	outNL      bool // stdout ends with a newline (or is empty)
	mode       int  // 0 idle, 1 text, 2 think
	textSeen   bool
	thinkBOL   bool
	thinkShown bool
	spin       chan struct{}
	spinDone   chan struct{}
	spinOn     bool
	sink       func(kind, text string) // session log: every line the harness shows
}

type colTracker struct {
	w   io.Writer
	ui  *UI
	out bool
}

func (c colTracker) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.ui.col0 = p[len(p)-1] == '\n'
		if c.out {
			c.ui.outNL = c.ui.col0
		}
	}
	return c.w.Write(p)
}

func NewUI(out, log io.Writer, mdColor, color, spinner bool) *UI {
	u := &UI{color: color, col0: true, outNL: true, spinOn: spinner}
	u.out = colTracker{out, u, true}
	u.log = colTracker{log, u, false}
	u.md = NewMarkdown(u.out, mdColor)
	return u
}

func (u *UI) c(code, s string) string {
	if !u.color {
		return s
	}
	return code + s + sgrReset
}

func (u *UI) nl() {
	if !u.col0 {
		io.WriteString(u.log, "\n")
	}
}

// BeginResponse starts the waiting spinner for a model request.
func (u *UI) BeginResponse(thinking bool) {
	u.mode, u.textSeen, u.thinkShown = 0, false, false
	if !u.spinOn {
		return
	}
	label := "menunggu model"
	if thinking {
		label = "berpikir"
	}
	u.spin, u.spinDone = make(chan struct{}), make(chan struct{})
	go func(stop, done chan struct{}) {
		defer close(done)
		frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
		start := time.Now()
		for i := 0; ; i++ {
			u.mu.Lock()
			fmt.Fprintf(u.log, "\r%s", u.c(sgrDim, fmt.Sprintf("%c %s %ds", frames[i%len(frames)], label, int(time.Since(start).Seconds()))))
			u.col0 = false
			u.mu.Unlock()
			select {
			case <-stop:
				u.mu.Lock()
				io.WriteString(u.log, "\r\x1b[K")
				u.col0 = true
				u.mu.Unlock()
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}(u.spin, u.spinDone)
}

func (u *UI) stopSpin() {
	if u.spin != nil {
		close(u.spin)
		<-u.spinDone
		u.spin = nil
	}
}

func (u *UI) Text(s string) {
	u.stopSpin()
	if u.mode == 2 {
		u.nl()
	}
	u.mode = 1
	if !u.textSeen {
		s = strings.TrimLeft(s, "\r\n")
		if s == "" {
			return
		}
		u.textSeen = true
	}
	u.md.Write(s)
}

func (u *UI) Think(s string) {
	u.stopSpin()
	if u.mode != 2 {
		u.nl()
		u.mode = 2
		u.thinkBOL = true
		s = strings.TrimLeft(s, "\r\n")
	}
	if u.quietThink {
		if !u.thinkShown {
			io.WriteString(u.log, u.c(sgrDim, "┊ berpikir…"))
			u.thinkShown = true
		}
		return
	}
	for s != "" {
		if u.thinkBOL {
			io.WriteString(u.log, u.c(sgrDim, "┊ "))
			u.thinkBOL = false
		}
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			io.WriteString(u.log, u.c(sgrDim+sgrItalic, s))
			return
		}
		io.WriteString(u.log, u.c(sgrDim+sgrItalic, s[:i])+"\n")
		u.thinkBOL = true
		s = s[i+1:]
	}
}

func (u *UI) EndResponse() {
	u.stopSpin()
	u.md.Flush()
	if u.mode == 1 && !u.outNL {
		io.WriteString(u.out, "\n")
	}
	u.nl()
	u.mode = 0
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n]) + "…"
	}
	return s
}

func (u *UI) ToolStart(name, summary string) {
	u.nl()
	fmt.Fprintf(u.log, "%s %s  %s\n", u.c(sgrCyan, "●"), u.c(sgrBold, name), clip(summary, 100))
}

func (u *UI) ToolDone(r ToolResult) {
	mark, col := "✓", sgrGreen
	if r.Failed {
		mark, col = "✗", sgrRed
	}
	if r.Invalid {
		mark, col = "✗", sgrYellow
		r.Status += " (koreksi dikirim)"
	}
	if r.Denied {
		mark, col = "⊘", sgrYellow
	}
	fmt.Fprintf(u.log, "  %s %s\n", u.c(col, mark), r.Status)
	for _, d := range r.Diff {
		dc := sgrDim
		if strings.HasPrefix(d, "- ") {
			dc = sgrRed
		} else if strings.HasPrefix(d, "+ ") {
			dc = sgrGreen
		}
		fmt.Fprintf(u.log, "  %s\n", u.c(dc, clip(d, 160)))
	}
	for _, d := range r.Detail {
		fmt.Fprintf(u.log, "  %s\n", u.c(sgrDim, d))
	}
}

// Options lists the choices of an ask_user call.
func (u *UI) Options(opts []string) {
	for i, o := range opts {
		fmt.Fprintf(u.log, "  %s %s\n", u.c(sgrCyan, fmt.Sprintf("%d)", i+1)), o)
	}
}

// MarkNewline records that something else (the line editor) left the cursor at
// the start of a line.
func (u *UI) MarkNewline() { u.col0 = true }

func (u *UI) Harness(msg string) {
	if u.sink != nil {
		u.sink("harness", msg)
	}
	u.nl()
	fmt.Fprintf(u.log, "%s\n", u.c(sgrYellow+sgrDim, "● [harness] "+msg))
}

func (u *UI) Info(msg string) {
	if u.sink != nil {
		u.sink("info", msg)
	}
	u.nl()
	fmt.Fprintf(u.log, "%s\n", u.c(sgrDim, msg))
}

func (u *UI) Warn(msg string) {
	if u.sink != nil {
		u.sink("warn", msg)
	}
	u.nl()
	fmt.Fprintf(u.log, "%s\n", u.c(sgrYellow, "! "+msg))
}

func (u *UI) Error(msg string) {
	if u.sink != nil {
		u.sink("error", msg)
	}
	u.nl()
	fmt.Fprintf(u.log, "%s\n", u.c(sgrRed, "✗ "+msg))
}

func (u *UI) Prompt(p string) {
	u.nl()
	io.WriteString(u.log, u.c(sgrYellow, p))
	u.col0 = true
}
