package rlog

import (
	"io"
	"log/slog"
	"os"
)

// ColorMode selects whether console text output is colored.
type ColorMode string

// Color modes for ConsoleSink.Color. The empty string means ColorNever.
const (
	ColorNever  ColorMode = "never"  // plain text
	ColorAuto   ColorMode = "auto"   // color only on a terminal, unless NO_COLOR is set or TERM=dumb
	ColorAlways ColorMode = "always" // always emit ANSI escapes (e.g. for a pager or a CI log viewer)
)

// ANSI escape sequences used by the text handler.
const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiBlue    = "\x1b[34m"
	ansiMagenta = "\x1b[35m"
	ansiCyan    = "\x1b[36m"
	ansiBoldRed = "\x1b[1;31m"
)

// levelColor returns the escape for a level: blue DEBUG, green INFO,
// yellow WARN, red ERROR, bold red above ERROR and magenta for NOTE
// (special messages).
func levelColor(l slog.Level) string {
	switch {
	case l == levelMessage:
		return ansiMagenta
	case l > slog.LevelError:
		return ansiBoldRed
	case l >= slog.LevelError:
		return ansiRed
	case l >= slog.LevelWarn:
		return ansiYellow
	case l >= slog.LevelInfo:
		return ansiGreen
	default:
		return ansiBlue
	}
}

// appendColored appends s wrapped in the escape code when color is set, or
// s alone otherwise.
func appendColored(buf []byte, color bool, code, s string) []byte {
	if !color {
		return append(buf, s...)
	}
	buf = append(buf, code...)
	buf = append(buf, s...)
	return append(buf, ansiReset...)
}

// useColor decides whether output to w should be colored under mode. On
// Windows it also switches the console into ANSI (virtual terminal) mode.
func useColor(w io.Writer, mode ColorMode) bool {
	switch mode {
	case ColorAlways:
		if f, ok := w.(*os.File); ok {
			enableVT(f)
		}
		return true
	case ColorAuto:
		if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
			return false
		}
		f, ok := w.(*os.File)
		if !ok {
			return false
		}
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false // redirected to a file or pipe
		}
		return enableVT(f)
	default:
		return false
	}
}
