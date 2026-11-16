package rlog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Time presets for Format.Time. Any other value is used as a Go time layout.
const (
	TimeFull        = "full"        // 2006-01-02 15:04:05.000 (the default)
	TimeDateTime    = "datetime"    // 2006-01-02 15:04:05
	TimeShort       = "short"       // 01-02 15:04:05
	TimeOnly        = "time"        // 15:04:05
	TimeMillis      = "timems"      // 15:04:05.000
	TimeDate        = "date"        // 2006-01-02
	TimeRFC3339     = "rfc3339"     // 2006-01-02T15:04:05Z07:00
	TimeRFC3339Nano = "rfc3339nano" // 2006-01-02T15:04:05.999999999Z07:00
	TimeISO8601     = "iso8601"     // 2006-01-02T15:04:05.000Z07:00
	TimeUnix        = "unix"        // seconds since 1970-01-01 UTC
	TimeUnixMilli   = "unixms"      // milliseconds since 1970-01-01 UTC
	TimeNone        = "none"        // no time at all
)

// timeLayouts maps the layout-based presets (lower case) to Go layouts.
// The empty string selects the default.
var timeLayouts = map[string]string{
	"":              "2006-01-02 15:04:05.000",
	TimeFull:        "2006-01-02 15:04:05.000",
	TimeDateTime:    time.DateTime,
	TimeShort:       "01-02 15:04:05",
	TimeOnly:        time.TimeOnly,
	TimeMillis:      "15:04:05.000",
	TimeDate:        time.DateOnly,
	TimeRFC3339:     time.RFC3339,
	TimeRFC3339Nano: time.RFC3339Nano,
	TimeISO8601:     "2006-01-02T15:04:05.000Z07:00",
}

// formatTime renders t according to f.Time and f.UTC. It returns "" for a
// zero time or the "none" preset.
func (f Format) formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	name := strings.ToLower(f.Time)
	switch name {
	case TimeNone:
		return ""
	case TimeUnix:
		return strconv.FormatInt(t.Unix(), 10)
	case TimeUnixMilli:
		return strconv.FormatInt(t.UnixMilli(), 10)
	}
	layout, ok := timeLayouts[name]
	if !ok {
		layout = f.Time // a custom Go layout
	}
	if f.UTC {
		t = t.UTC()
	} else {
		t = t.Local()
	}
	return t.Format(layout)
}

// jsonTime returns the time attribute for JSON output: a number for the
// unix presets, a formatted string otherwise, and an empty Attr (which slog
// drops) for "none".
func (f Format) jsonTime(t time.Time) slog.Attr {
	switch strings.ToLower(f.Time) {
	case TimeUnix:
		return slog.Int64(slog.TimeKey, t.Unix())
	case TimeUnixMilli:
		return slog.Int64(slog.TimeKey, t.UnixMilli())
	}
	s := f.formatTime(t)
	if s == "" {
		return slog.Attr{}
	}
	return slog.String(slog.TimeKey, s)
}

// formatSource renders file:line according to f.Source, or "" when sources
// are off.
func (f Format) formatSource(file string, line int) string {
	switch f.Source {
	case SourceShort:
		file = path.Base(filepath.ToSlash(file))
	case SourceFull:
	default:
		return ""
	}
	return file + ":" + strconv.Itoa(line)
}

// sourceOf resolves a record's program counter to a formatted file:line.
func (f Format) sourceOf(pc uintptr) string {
	if f.Source == SourceNone || pc == 0 {
		return ""
	}
	fr, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	if fr.File == "" {
		return ""
	}
	return f.formatSource(fr.File, fr.Line)
}

// newWriterHandler builds the console/file handler: rlog's text format
// (optionally colored), or slog's JSON handler with the same time, source
// and field options.
func newWriterHandler(w io.Writer, asJSON, color bool, level slog.Leveler, f Format, program string) slog.Handler {
	if asJSON {
		return newJSONHandler(w, level, f, program)
	}
	var mu sync.Mutex // one whole line per write, so records never interleave
	return &textHandler{
		level:  level,
		format: f,
		full:   true,
		color:  color,
		prog:   program,
		emit: func(_ slog.Level, _ time.Time, line string) error {
			mu.Lock()
			defer mu.Unlock()
			_, err := io.WriteString(w, line+"\n")
			return err
		},
	}
}

// newJSONHandler wraps slog.JSONHandler, rewriting the built-in time, level
// and source attributes to follow f, and adding a "program" key when
// f.Program is set. The type checks keep a user's own attribute that
// happens to be called "time", "level" or "source" untouched.
func newJSONHandler(w io.Writer, level slog.Leveler, f Format, program string) slog.Handler {
	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: f.Source != SourceNone,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				if a.Value.Kind() == slog.KindTime {
					return f.jsonTime(a.Value.Time())
				}
			case slog.LevelKey:
				if l, ok := a.Value.Any().(slog.Level); ok {
					if !f.Level {
						return slog.Attr{}
					}
					if l == levelNote {
						return slog.String(slog.LevelKey, noteLabel)
					}
				}
			case slog.SourceKey:
				if src, ok := a.Value.Any().(*slog.Source); ok {
					if src.File == "" { // no caller, e.g. a notification
						return slog.Attr{}
					}
					return slog.String(slog.SourceKey, f.formatSource(src.File, src.Line))
				}
			case ComponentKey:
				if !f.Component {
					return slog.Attr{}
				}
			}
			return a
		},
	}
	var h slog.Handler = slog.NewJSONHandler(w, opts)
	if f.Program && program != "" {
		h = h.WithAttrs([]slog.Attr{slog.String("program", program)})
	}
	return h
}

// textHandler formats records as single lines:
//
//	2026-11-16 10:04:05.123 INFO  lutron: [bridge] connected host=10.0.0.5 (bridge.go:42)
//
// With full=false it produces the short body sent to the services instead:
//
//	[bridge] connected host=10.0.0.5
//
// The formatted line is handed to emit, which writes or enqueues it.
type textHandler struct {
	level  slog.Leveler // minimum level
	format Format       // layout options
	full   bool         // include time, level and program
	color  bool         // add ANSI colors (full mode only)
	prog   string       // program name for the prefix
	emit   func(level slog.Level, t time.Time, line string) error

	component string // from a "component" attr added with With
	attrs     []byte // attrs added with With, already formatted
	group     string // open groups as a "g1.g2." key prefix
}

// Enabled reports whether l is at or above the handler's level.
func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

// WithAttrs returns a handler with attrs pre-formatted. A top-level
// component attribute is kept apart so it can be shown as "[name]".
func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := *h
	h2.attrs = append([]byte(nil), h.attrs...)
	for _, a := range attrs {
		if h.group == "" && a.Key == ComponentKey {
			h2.component = a.Value.Resolve().String()
			continue
		}
		h2.attrs = appendAttr(h2.attrs, h.group, a, h.color && h.full)
	}
	return &h2
}

// WithGroup returns a handler that prefixes later attribute keys with name.
func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.group = h.group + name + "."
	return &h2
}

// Handle formats r as one line and passes it to emit.
func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	color := h.color && h.full
	comp := h.component
	var attrs []byte
	r.Attrs(func(a slog.Attr) bool {
		if h.group == "" && a.Key == ComponentKey {
			comp = a.Value.Resolve().String()
			return true
		}
		attrs = appendAttr(attrs, h.group, a, color)
		return true
	})

	f := h.format
	buf := make([]byte, 0, 128+len(r.Message)+len(h.attrs)+len(attrs))
	if h.full {
		if ts := f.formatTime(r.Time); ts != "" {
			buf = appendColored(buf, color, ansiDim, ts)
			buf = append(buf, ' ')
		}
		if f.Level {
			buf = appendColored(buf, color, levelColor(r.Level), fmt.Sprintf("%-5s", levelName(r.Level)))
			buf = append(buf, ' ')
		}
		if f.Program && h.prog != "" {
			buf = appendColored(buf, color, ansiBold, h.prog+":")
			buf = append(buf, ' ')
		}
	}
	if f.Component && comp != "" {
		buf = appendColored(buf, color, ansiCyan, "["+comp+"]")
		buf = append(buf, ' ')
	}
	buf = append(buf, r.Message...)
	buf = append(buf, h.attrs...)
	buf = append(buf, attrs...)
	if src := f.sourceOf(r.PC); src != "" {
		buf = append(buf, ' ')
		buf = appendColored(buf, color, ansiDim, "("+src+")")
	}
	return h.emit(r.Level, r.Time, string(buf))
}

// levelName is the level's text: "NOTE" for notifications, the slog name
// (DEBUG, INFO, WARN, ERROR, ERROR+4, ...) otherwise.
func levelName(l slog.Level) string {
	if l == levelNote {
		return noteLabel
	}
	return l.String()
}

// appendAttr appends " key=value", flattening groups into dotted keys.
// With color, keys are dimmed and error values shown in red.
func appendAttr(buf []byte, prefix string, a slog.Attr, color bool) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}
	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			buf = appendAttr(buf, prefix, ga, color)
		}
		return buf
	}
	// Value.String formats like fmt.Sprint, so a value whose Error or
	// String method panics (a nil pointer, say) cannot crash the logger.
	s, isErr := a.Value.String(), false
	switch a.Value.Kind() {
	case slog.KindTime:
		s = a.Value.Time().Format(time.RFC3339Nano)
	case slog.KindAny:
		_, isErr = a.Value.Any().(error)
	}
	buf = append(buf, ' ')
	buf = appendColored(buf, color, ansiDim, quoted(prefix+a.Key)+"=")
	return appendColored(buf, color && isErr, ansiRed, quoted(s))
}

// quoted returns s, in double quotes when it is empty or contains spaces,
// quotes, '=' or unprintable characters, so key=value stays parseable.
func quoted(s string) string {
	quote := s == ""
	for _, r := range s {
		if r == '=' || r == '"' || r == utf8.RuneError || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			quote = true
			break
		}
	}
	if quote {
		return strconv.Quote(s)
	}
	return s
}

// formatArgs renders msg followed by slog-style key/value args as
// "msg key=value ...", the same way records are rendered.
func formatArgs(msg string, args ...any) string {
	if len(args) == 0 {
		return msg
	}
	r := slog.NewRecord(time.Time{}, 0, msg, 0)
	r.Add(args...)
	buf := []byte(msg)
	r.Attrs(func(a slog.Attr) bool {
		buf = appendAttr(buf, "", a, false)
		return true
	})
	return string(buf)
}
