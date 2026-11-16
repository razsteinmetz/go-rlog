// Package rlog supplies a log/slog handler that fans records out to several
// sinks: console, rotating file, Telegram, Pushover and email.
//
// Programs log with plain slog calls; rlog only builds the handler:
//
//	cfg := rlog.DefaultConfig()
//	cfg.Program = "lutron"
//	cfg.File.Enabled = true
//	cfg.File.Path = "/var/log/lutron.log"
//	l, err := rlog.Init(cfg) // also installs it as slog.Default
//	if err != nil { ... }
//	defer l.Close()
//
//	slog.Info("connected", "host", host)
//	log := rlog.Component("bridge")
//	log.Warn("retrying", "err", err)
//
// The configuration can also be read from a YAML or JSON file with
// LoadConfig, or in one step with InitFile.
//
// rlog carries two kinds of output. Log records come from slog calls; each
// sink has its own Enabled switch and minimum Level. Notifications are
// level-less messages sent with Notify, NotifyRaw and Send: to every sink
// by default (console and file show them as NOTE lines), or to the sinks
// named in Message.To. A sink's Notifications setting decides whether it
// accepts them.
//
// Whatever goes to the services (Telegram, Pushover, email) is queued and
// sent on background goroutines, so a slow or unreachable service never
// blocks the caller, and Close waits at most Config.CloseTimeout for the
// queues to empty.
package rlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ComponentKey is the attribute key rlog treats as the component name.
// rlog.Component(name) is shorthand for slog.Default().With(ComponentKey, name).
const ComponentKey = "component"

// RepeatedKey is the attribute rlog adds to the summary record emitted when
// Config.Repeats suppressed repeats of a record. Its value is the number of
// repeats that were suppressed.
const RepeatedKey = "repeated"

// SourceMode selects how the caller's file:line is shown.
type SourceMode string

// Source modes for Format.Source.
const (
	SourceNone  SourceMode = ""      // no source location
	SourceShort SourceMode = "short" // file.go:42
	SourceFull  SourceMode = "full"  // /full/path/to/file.go:42
)

// Format controls how console and file records look. The services always
// use their own short format (see Telegram, Pushover, Email).
type Format struct {
	// Time is a preset name (see TimeFull and the other Time constants:
	// "full", "datetime", "short", "time", "timems", "date", "rfc3339",
	// "rfc3339nano", "iso8601", "unix", "unixms", "none") or a Go time
	// layout such as "Jan _2 15:04:05". Empty means "full"
	// (2006-01-02 15:04:05.000).
	Time string `json:"time" yaml:"time"`
	// UTC prints times in UTC instead of local time. It also makes daily
	// file rotation happen at UTC midnight.
	UTC bool `json:"utc" yaml:"utc"`
	// Source selects whether and how the caller's file:line is shown.
	Source SourceMode `json:"source" yaml:"source"`
	// Level shows the record's level.
	Level bool `json:"level" yaml:"level"`
	// Component shows the component attribute as "[name]" in text output;
	// in JSON output it keeps (true) or drops (false) the "component" key.
	Component bool `json:"component" yaml:"component"`
	// Program prefixes text lines with Config.Program; in JSON output it
	// adds a "program" key.
	Program bool `json:"program" yaml:"program"`
}

// ConsoleSink writes to stderr (or Writer).
type ConsoleSink struct {
	// Enabled turns the sink on.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Level is the minimum level written.
	Level slog.Level `json:"level" yaml:"level"`
	// JSON writes one JSON object per record instead of a text line.
	JSON bool `json:"json" yaml:"json"`
	// Notifications controls whether notifications (Notify, NotifyRaw,
	// Send) are written here, as NOTE lines. Nil (not set) means yes.
	Notifications *bool `json:"notifications" yaml:"notifications"`
	// Color colors text output with ANSI escapes: "never" (or empty),
	// "auto" (only when writing to a terminal and NO_COLOR is unset) or
	// "always". It is ignored for JSON output.
	Color ColorMode `json:"color" yaml:"color"`
	// Writer receives the output. Default os.Stderr.
	Writer io.Writer `json:"-" yaml:"-"`
}

// FileSink writes to a file rotated by lumberjack.
type FileSink struct {
	// Enabled turns the sink on.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Level is the minimum level written.
	Level slog.Level `json:"level" yaml:"level"`
	// JSON writes one JSON object per record instead of a text line.
	JSON bool `json:"json" yaml:"json"`
	// Notifications controls whether notifications (Notify, NotifyRaw,
	// Send) are written here, as NOTE lines. Nil (not set) means yes.
	Notifications *bool `json:"notifications" yaml:"notifications"`
	// Path is the log file. Missing directories are created.
	Path string `json:"path" yaml:"path"`
	// MaxSizeMB rotates when the file reaches this size. 0 means 100 MB;
	// size rotation cannot be turned off.
	MaxSizeMB int `json:"max_size_mb" yaml:"max_size_mb"`
	// Daily also rotates at midnight (local, or UTC when Format.UTC is set).
	// The rotation happens only while the program is running at midnight.
	Daily bool `json:"daily" yaml:"daily"`
	// MaxBackups is how many rotated files to keep; 0 keeps all.
	MaxBackups int `json:"max_backups" yaml:"max_backups"`
	// MaxAgeDays deletes rotated files older than this many days; 0 never.
	MaxAgeDays int `json:"max_age_days" yaml:"max_age_days"`
	// Compress gzips rotated files.
	Compress bool `json:"compress" yaml:"compress"`
}

// RepeatConfig groups identical records that arrive close together.
//
// When enabled, the first occurrence of a record is logged immediately.
// Identical records (same level, message, component and attributes; the
// time and source location are ignored) that follow within Window are
// counted instead of logged. When the window ends, the last of them is
// logged once, stamped with the current time and with a RepeatedKey
// attribute holding the count, and a new window starts; a window with no
// repeats ends the grouping. A steady flood of one message therefore
// produces one line per Window. Notifications are grouped the same way.
type RepeatConfig struct {
	// Enabled turns grouping on.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Window is the grouping period. 0 means one minute.
	Window Duration `json:"window" yaml:"window"`
}

// Config describes the whole logger. Start from DefaultConfig and change what
// you need. LoadConfig, ParseYAML and ParseJSON already start from
// DefaultConfig; when decoding into a Config yourself, decode on top of
// DefaultConfig() so the boolean defaults are kept.
type Config struct {
	// Program names the program in log lines (Format.Program) and in the
	// titles sent to the services. Empty means the executable's name.
	Program string `json:"program" yaml:"program"`
	// Format controls the console and file output.
	Format Format `json:"format" yaml:"format"`
	// Console is the console sink.
	Console ConsoleSink `json:"console" yaml:"console"`
	// File is the rotating file sink.
	File FileSink `json:"file" yaml:"file"`
	// Telegram is the Telegram sink.
	Telegram TelegramSink `json:"telegram" yaml:"telegram"`
	// Pushover is the Pushover sink.
	Pushover PushoverSink `json:"pushover" yaml:"pushover"`
	// Email is the SMTP sink.
	Email EmailSink `json:"email" yaml:"email"`
	// Repeats groups identical records sent close together.
	Repeats RepeatConfig `json:"repeats" yaml:"repeats"`

	// QueueSize is how many records and notifications each service
	// (Telegram, Pushover, email) buffers before it drops new ones. 0 means
	// 100.
	QueueSize int `json:"queue_size" yaml:"queue_size"`
	// NotifyTimeout bounds one delivery attempt (connect, send, reply) to a
	// service. A delivery that fails is reported to ErrorWriter and
	// dropped; it is not retried. 0 means 15 seconds.
	NotifyTimeout Duration `json:"notify_timeout" yaml:"notify_timeout"`
	// CloseTimeout is the longest Close waits, in total, for the services'
	// queues to empty. Whatever is still queued afterwards is dropped. 0
	// means 5 seconds.
	CloseTimeout Duration `json:"close_timeout" yaml:"close_timeout"`
	// ErrorWriter receives rlog's own errors, such as a failed Telegram
	// request. It is never routed through slog, and repeated errors from one
	// service are throttled to one line per minute. Default os.Stderr.
	ErrorWriter io.Writer `json:"-" yaml:"-"`
}

// Defaults used when the corresponding Config fields are zero.
const (
	defaultQueueSize     = 100
	defaultNotifyTimeout = 15 * time.Second
	defaultCloseTimeout  = 5 * time.Second
	defaultRepeatWindow  = time.Minute
	defaultMaxSizeMB     = 100
)

// positive returns v when it is greater than zero, and def otherwise.
func positive[T int | time.Duration](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}

// DefaultConfig returns a colored console logger at Info with level,
// component and program shown; a file sink (off until Enabled and Path are
// set) at Info that rotates daily and at 100 MB, keeps 7 compressed
// backups; the services off with level Error; and repeat grouping off with
// a one-minute window.
func DefaultConfig() Config {
	return Config{
		Format: Format{
			Time:      TimeFull,
			Level:     true,
			Component: true,
			Program:   true,
		},
		Console: ConsoleSink{Enabled: true, Level: slog.LevelInfo, Color: ColorAuto},
		File: FileSink{
			Level:      slog.LevelInfo,
			MaxSizeMB:  defaultMaxSizeMB,
			Daily:      true,
			MaxBackups: 7,
			Compress:   true,
		},
		Telegram:      TelegramSink{Level: slog.LevelError},
		Pushover:      PushoverSink{Level: slog.LevelError},
		Email:         EmailSink{Level: slog.LevelError},
		Repeats:       RepeatConfig{Window: Duration(defaultRepeatWindow)},
		QueueSize:     defaultQueueSize,
		NotifyTimeout: Duration(defaultNotifyTimeout),
		CloseTimeout:  Duration(defaultCloseTimeout),
	}
}

// Logger owns the handler and the resources behind it (the open file, the
// services' queues, repeat timers). Close it before the program exits so
// queued deliveries are sent and grouped repeats are flushed.
//
// All methods are safe for concurrent use. Close, Send, Notify and
// NotifyRaw do nothing on a nil *Logger, so the package-level helpers are
// safe to call before Init.
type Logger struct {
	handler      slog.Handler  // fan-out (optionally wrapped by repeat grouping)
	logger       *slog.Logger  // slog.New(handler)
	program      string        // resolved Config.Program
	outputs      []output      // sinks that accept notifications, for Send
	dispatchers  []*dispatcher // the services' queues, drained by Close
	repeats      *repeatState  // nil when Config.Repeats is off
	closeTimeout time.Duration // total wait for the queues in Close
	fileClose    func() error  // closes the log file; nil without a file sink
	closeOnce    sync.Once     // makes Close idempotent
	closeErr     error         // result of the first Close
}

// std is the Logger installed by Init; the package-level Notify, NotifyRaw
// and Send functions use it.
var std atomic.Pointer[Logger]

// New builds a Logger from cfg. It does not touch slog.Default; see Init.
//
// New fails if a sink's configuration is invalid (for example a Telegram
// sink without a token) or the log file cannot be opened. It never contacts
// the services, so a bad API key or a missing network only shows up later,
// as errors written to Config.ErrorWriter.
func New(cfg Config) (*Logger, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	errw := cfg.ErrorWriter
	if errw == nil {
		errw = os.Stderr
	}
	program := cfg.Program
	if program == "" {
		program = strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	}
	queueSize := positive(cfg.QueueSize, defaultQueueSize)
	notifyTimeout := positive(time.Duration(cfg.NotifyTimeout), defaultNotifyTimeout)

	l := &Logger{
		program:      program,
		closeTimeout: positive(time.Duration(cfg.CloseTimeout), defaultCloseTimeout),
	}
	var handlers []slog.Handler
	// addOutput makes a sink receive notifications, unless its config says no.
	addOutput := func(on *bool, target Target, deliver func(delivery)) {
		if notificationsOn(on) {
			l.outputs = append(l.outputs, output{target, deliver})
		}
	}
	fail := func(err error) (*Logger, error) {
		_ = l.Close()
		return nil, err
	}

	if c := cfg.Console; c.Enabled {
		w := c.Writer
		if w == nil {
			w = os.Stderr
		}
		h := newWriterHandler(w, c.JSON, !c.JSON && useColor(w, c.Color), c.Level, cfg.Format, program)
		handlers = append(handlers, h)
		addOutput(c.Notifications, ToConsole, noteWriter(h, c.JSON))
	}

	if c := cfg.File; c.Enabled {
		w, closeFn, err := openFile(c, cfg.Format.UTC)
		if err != nil {
			return fail(err)
		}
		l.fileClose = closeFn
		h := newWriterHandler(w, c.JSON, false, c.Level, cfg.Format, program)
		handlers = append(handlers, h)
		addOutput(c.Notifications, ToFile, noteWriter(h, c.JSON))
	}

	// The three services differ only in these settings and in their sender.
	services := []struct {
		name          string
		target        Target
		enabled       bool
		level         slog.Level
		messagesOnly  bool
		notifications *bool
		build         func() (sender, error)
	}{
		{"telegram", ToTelegram, cfg.Telegram.Enabled, cfg.Telegram.Level, cfg.Telegram.MessagesOnly, cfg.Telegram.Notifications, cfg.Telegram.sender},
		{"pushover", ToPushover, cfg.Pushover.Enabled, cfg.Pushover.Level, cfg.Pushover.MessagesOnly, cfg.Pushover.Notifications, cfg.Pushover.sender},
		{"email", ToEmail, cfg.Email.Enabled, cfg.Email.Level, cfg.Email.MessagesOnly, cfg.Email.Notifications, cfg.Email.sender},
	}
	for _, s := range services {
		if !s.enabled {
			continue
		}
		snd, err := s.build()
		if err != nil {
			return fail(fmt.Errorf("rlog: %s: %w", s.name, err))
		}
		d := newDispatcher(s.name, snd, queueSize, notifyTimeout, errw)
		l.dispatchers = append(l.dispatchers, d)
		addOutput(s.notifications, s.target, d.enqueue)
		if !s.messagesOnly {
			handlers = append(handlers, newNotifyHandler(d, s.level, cfg.Format, program))
		}
	}

	l.handler = fanout(handlers)
	if cfg.Repeats.Enabled {
		l.repeats = newRepeatState(positive(time.Duration(cfg.Repeats.Window), defaultRepeatWindow))
		l.handler = &repeatHandler{next: l.handler, st: l.repeats}
	}
	l.logger = slog.New(l.handler)
	return l, nil
}

// validate checks the enumerated string options so that a typo in a config
// file is reported instead of silently ignored.
func (cfg *Config) validate() error {
	switch cfg.Format.Source {
	case SourceNone, SourceShort, SourceFull:
	default:
		return fmt.Errorf("rlog: format.source: unknown mode %q (want \"\", short or full)", cfg.Format.Source)
	}
	switch cfg.Console.Color {
	case "", ColorNever, ColorAuto, ColorAlways:
	default:
		return fmt.Errorf("rlog: console.color: unknown mode %q (want never, auto or always)", cfg.Console.Color)
	}
	return nil
}

// Init calls New and installs the result as slog.Default and as the Logger
// used by the package-level Notify, NotifyRaw and Send functions.
func Init(cfg Config) (*Logger, error) {
	l, err := New(cfg)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(l.logger)
	std.Store(l)
	return l, nil
}

// InitFile loads the configuration from path (see LoadConfig) and calls Init.
func InitFile(path string) (*Logger, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return Init(cfg)
}

// Default returns the Logger installed by the most recent Init, or nil.
func Default() *Logger { return std.Load() }

// Handler returns the slog.Handler that feeds every enabled sink.
func (l *Logger) Handler() slog.Handler { return l.handler }

// Slog returns a *slog.Logger using Handler.
func (l *Logger) Slog() *slog.Logger { return l.logger }

// Close flushes grouped repeats, waits up to Config.CloseTimeout for the
// services' queues to empty, and closes the log file. It returns an error
// if deliveries were still pending when the timeout ran out.
//
// Close is idempotent. Records logged after Close still reach the console
// and file sinks; nothing more is sent to the services.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		// Flush repeat summaries first so they still reach the services.
		if l.repeats != nil {
			l.repeats.close()
		}
		// Drain all queues in parallel against one shared deadline.
		deadline := time.Now().Add(l.closeTimeout)
		errs := make([]error, len(l.dispatchers)+1)
		var wg sync.WaitGroup
		for i, d := range l.dispatchers {
			wg.Go(func() { errs[i] = d.close(deadline) })
		}
		wg.Wait()
		if l.fileClose != nil {
			errs[len(errs)-1] = l.fileClose()
		}
		l.closeErr = errors.Join(errs...)
	})
	return l.closeErr
}

// Component returns slog.Default() with the component attribute set.
// Call it after Init so it picks up rlog's handler.
func Component(name string) *slog.Logger {
	return slog.Default().With(ComponentKey, name)
}

// fanout sends each record to every handler that accepts its level.
type fanout []slog.Handler

// Enabled reports whether any handler accepts level.
func (f fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle passes a clone of r to every handler that accepts its level and
// joins their errors.
func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

// WithAttrs returns a fanout whose handlers all carry attrs.
func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

// WithGroup returns a fanout whose handlers all have the group open.
func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}
