//go:build windows

package rlog

import (
	"os"
	"syscall"
)

// enableVirtualTerminalProcessing is the console mode flag that makes the
// Windows console interpret ANSI escape sequences.
const enableVirtualTerminalProcessing = 0x0004

// procSetConsoleMode is kernel32's SetConsoleMode, which the syscall package
// does not export.
var procSetConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

// enableVT turns on ANSI escape processing for the console behind f. It
// reports false when f is not a console (for example the NUL device) or the
// console cannot be switched (Windows versions before 10).
func enableVT(f *os.File) bool {
	h := syscall.Handle(f.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
