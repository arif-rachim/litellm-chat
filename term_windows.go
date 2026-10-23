package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Terminal control on Windows: console modes through kernel32. With virtual
// terminal input the console hands over the same escape sequences a Linux
// terminal does, so the editor's key parsing is shared; with virtual terminal
// processing the ANSI colors and cursor moves render as they do on Linux.

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procFlushConsoleInputBuffer    = kernel32.NewProc("FlushConsoleInputBuffer")
)

const (
	enableProcessedInput            = 0x0001
	enableLineInput                 = 0x0002
	enableEchoInput                 = 0x0004
	enableVirtualTerminalProcessing = 0x0004
	enableVirtualTerminalInput      = 0x0200
)

func init() {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		var m uint32
		if syscall.GetConsoleMode(syscall.Handle(f.Fd()), &m) == nil {
			setConsoleMode(f.Fd(), m|enableVirtualTerminalProcessing)
		}
	}
}

func setConsoleMode(h uintptr, m uint32) bool {
	r, _, _ := procSetConsoleMode.Call(h, uintptr(m))
	return r != 0
}

type termState struct {
	fd  uintptr
	old uint32
}

func rawMode(f *os.File) *termState {
	if f == nil {
		return nil
	}
	var m uint32
	if syscall.GetConsoleMode(syscall.Handle(f.Fd()), &m) != nil {
		return nil
	}
	raw := m&^(enableProcessedInput|enableLineInput|enableEchoInput) | enableVirtualTerminalInput
	if !setConsoleMode(f.Fd(), raw) {
		return nil
	}
	return &termState{f.Fd(), m}
}

func (s *termState) restore() {
	if s != nil {
		setConsoleMode(s.fd, s.old)
	}
}

// isTTY reports whether f is a console. Pipes and files have no console mode.
func isTTY(f *os.File) bool {
	var m uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &m) == nil
}

type consoleInfo struct {
	size, cursor             [2]int16
	attr                     uint16
	left, top, right, bottom int16
	maxSize                  [2]int16
}

// termWidth is the console window's column count, 0 when unknown. The width
// belongs to the output buffer, so stdout is asked whatever f is.
func termWidth(*os.File) int {
	var ci consoleInfo
	if r, _, _ := procGetConsoleScreenBufferInfo.Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&ci))); r == 0 {
		return 0
	}
	return int(ci.right-ci.left) + 1
}

// flushInput drops what was typed but not read yet.
func flushInput(f *os.File) {
	if f != nil {
		procFlushConsoleInputBuffer.Call(f.Fd())
	}
}
