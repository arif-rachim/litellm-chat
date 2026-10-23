package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Terminal control on Linux: termios through ioctl. Raw mode is entered only
// while reading, so Ctrl+C keeps working as SIGINT everywhere else.

type termState struct {
	fd  uintptr
	old syscall.Termios
}

func rawMode(f *os.File) *termState {
	if f == nil {
		return nil
	}
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

// isTTY reports whether f is a terminal. A character-device check is not
// enough: /dev/null is one too.
func isTTY(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

type winsize struct{ row, col, xpixel, ypixel uint16 }

// termWidth is the terminal's column count, 0 when unknown.
func termWidth(f *os.File) int {
	var ws winsize
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); err != 0 {
		return 0
	}
	return int(ws.col)
}

// tcflsh is the TCFLSH ioctl, which package syscall does not export. Its
// value is the same on amd64 and arm64, the architectures released.
const tcflsh = 0x540B

// flushInput drops what was typed but not read yet (tcflush TCIFLUSH).
func flushInput(f *os.File) {
	if f != nil {
		syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), tcflsh, 0) // 0 = TCIFLUSH
	}
}
