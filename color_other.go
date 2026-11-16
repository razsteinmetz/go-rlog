//go:build !windows

package rlog

import "os"

// enableVT reports whether f can show ANSI colors. Terminals outside
// Windows always interpret ANSI escapes, so there is nothing to switch on.
func enableVT(*os.File) bool { return true }
