package rlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes made by
// timers and dispatcher goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p under the lock.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the contents under the lock.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newBuf builds a Logger that writes Debug and above to a buffer, after
// letting mutate adjust the config. The Logger is closed at test cleanup.
func newBuf(t *testing.T, mutate func(*Config)) (*slog.Logger, *bytes.Buffer, *Logger) {
	t.Helper()
	var buf bytes.Buffer
	cfg := DefaultConfig()
	cfg.Program = "prog"
	cfg.Console.Writer = &buf
	cfg.Console.Level = slog.LevelDebug
	if mutate != nil {
		mutate(&cfg)
	}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l.Slog(), &buf, l
}

// TestTextFormat checks the full text line layout.
func TestTextFormat(t *testing.T) {
	log, buf, _ := newBuf(t, func(c *Config) { c.Format.Source = SourceShort })
	log.With(ComponentKey, "bridge").Info("connected", "host", "10.0.0.5", "msg", "two words", "err", errors.New("boom"))
	re := regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{3} INFO  prog: \[bridge\] connected host=10\.0\.0\.5 msg="two words" err=boom \(rlog_test\.go:\d+\)\n$`)
	if !re.MatchString(buf.String()) {
		t.Fatalf("got %q", buf.String())
	}
}

// TestFormatOptions checks that the Format switches remove fields and that
// groups flatten into dotted keys.
func TestFormatOptions(t *testing.T) {
	log, buf, _ := newBuf(t, func(c *Config) {
		c.Format = Format{Time: "none"}
	})
	log.Info("hi", ComponentKey, "x", slog.Group("g", "a", 1))
	log.WithGroup("req").With("id", 7).Info("grouped")
	want := "hi g.a=1\ngrouped req.id=7\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}

// TestTimeUTCAndRFC3339 checks UTC conversion with an RFC 3339 layout.
func TestTimeUTCAndRFC3339(t *testing.T) {
	f := Format{Time: "rfc3339", UTC: true}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 2*3600))
	if got := f.formatTime(ts); got != "2026-01-02T01:04:05Z" {
		t.Fatal(got)
	}
}

// TestTimePresets checks every named time preset and a custom layout.
func TestTimePresets(t *testing.T) {
	ts := time.Date(2026, 3, 4, 5, 6, 7, 890_000_000, time.UTC)
	cases := map[string]string{
		"":             "2026-03-04 05:06:07.890",
		TimeFull:       "2026-03-04 05:06:07.890",
		"FULL":         "2026-03-04 05:06:07.890",
		TimeDateTime:   "2026-03-04 05:06:07",
		TimeShort:      "03-04 05:06:07",
		TimeOnly:       "05:06:07",
		TimeMillis:     "05:06:07.890",
		TimeDate:       "2026-03-04",
		TimeISO8601:    "2026-03-04T05:06:07.890Z",
		TimeUnix:       "1772600767",
		TimeUnixMilli:  "1772600767890",
		TimeNone:       "",
		"Jan _2 15:04": "Mar  4 05:06",
	}
	for preset, want := range cases {
		f := Format{Time: preset, UTC: true}
		if got := f.formatTime(ts); got != want {
			t.Errorf("%q: got %q want %q", preset, got, want)
		}
	}
}

// TestLevels checks the console level filter.
func TestLevels(t *testing.T) {
	log, buf, _ := newBuf(t, func(c *Config) { c.Console.Level = slog.LevelWarn })
	log.Info("no")
	log.Warn("yes")
	if strings.Contains(buf.String(), "no") || !strings.Contains(buf.String(), "yes") {
		t.Fatalf("got %q", buf.String())
	}
}

// TestColor checks that ColorAlways colors the level, component and keys,
// and that a non-terminal writer gets no color under ColorAuto.
func TestColor(t *testing.T) {
	log, buf, _ := newBuf(t, func(c *Config) {
		c.Format.Time = TimeNone
		c.Console.Color = ColorAlways
	})
	log.With(ComponentKey, "b").Error("bad", "n", 1)
	want := ansiRed + "ERROR" + ansiReset + " " + ansiBold + "prog:" + ansiReset + " " +
		ansiCyan + "[b]" + ansiReset + " bad " + ansiDim + "n=" + ansiReset + "1\n"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}

	log2, buf2, _ := newBuf(t, func(c *Config) { c.Console.Color = ColorAuto })
	log2.Info("plain")
	if strings.Contains(buf2.String(), "\x1b[") {
		t.Fatalf("auto colored a buffer: %q", buf2.String())
	}

	if _, err := New(Config{Console: ConsoleSink{Enabled: true, Color: "rainbow"}}); err == nil {
		t.Fatal("expected error for unknown color mode")
	}
}

// TestJSON checks JSON output with the Format options applied.
func TestJSON(t *testing.T) {
	log, buf, _ := newBuf(t, func(c *Config) {
		c.Console.JSON = true
		c.Console.Color = ColorAlways // ignored for JSON
		c.Format.Time = "rfc3339"
		c.Format.UTC = true
		c.Format.Source = SourceShort
	})
	log.With(ComponentKey, "bridge").Error("bad", "n", 3)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err, buf.String())
	}
	if m["program"] != "prog" || m["component"] != "bridge" || m["level"] != "ERROR" || m["n"] != 3.0 {
		t.Fatalf("got %v", m)
	}
	if !strings.HasSuffix(m["time"].(string), "Z") || !strings.HasPrefix(m["source"].(string), "rlog_test.go:") {
		t.Fatalf("got %v", m)
	}
}

// TestJSONUnixTime checks that the unix presets produce numeric times in JSON.
func TestJSONUnixTime(t *testing.T) {
	log, buf, _ := newBuf(t, func(c *Config) {
		c.Console.JSON = true
		c.Format.Time = TimeUnixMilli
	})
	log.Info("x")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err, buf.String())
	}
	if v, ok := m["time"].(float64); !ok || v < 1e12 {
		t.Fatalf("got %v", m["time"])
	}
}

// TestConfigJSON checks decoding JSON straight into a Config.
func TestConfigJSON(t *testing.T) {
	cfg := DefaultConfig()
	in := `{"console":{"level":"warn"},"pushover":{"priorities":{"WARN":0,"ERROR+4":2}}}`
	if err := json.Unmarshal([]byte(in), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Console.Level != slog.LevelWarn || cfg.Pushover.Priorities[slog.LevelError+4] != 2 || !cfg.Format.Level {
		t.Fatalf("%+v", cfg)
	}
}

// TestParseYAML checks levels, durations, maps, defaults, environment
// expansion and unknown-key detection in YAML configs.
func TestParseYAML(t *testing.T) {
	t.Setenv("RLOG_TEST_TOKEN", `abc"#: x`) // YAML-hostile characters
	in := `
program: lutron
format:
  time: short
  source: short
console:
  level: debug
  color: never
file:
  enabled: true
  path: logs/app.log
  max_backups: 14
repeats:
  enabled: true
  window: 30s
notify_timeout: 2s
telegram:
  enabled: true
  token: ${RLOG_TEST_TOKEN}
  chat_id: -100123
  messages_only: true
pushover:
  priorities:
    WARN: 0
    ERROR+4: 2
email:
  host: ${RLOG_TEST_UNSET:-smtp.example.com}
  to: [a@example.com, b@example.com]
`
	cfg, err := ParseYAML([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case cfg.Program != "lutron", cfg.Format.Time != TimeShort, cfg.Format.Source != SourceShort:
		t.Fatalf("format %+v", cfg)
	case cfg.Console.Level != slog.LevelDebug, cfg.Console.Color != ColorNever:
		t.Fatalf("console %+v", cfg.Console)
	case !cfg.File.Enabled, cfg.File.MaxBackups != 14, !cfg.File.Daily, cfg.File.MaxSizeMB != 100, !cfg.File.Compress:
		t.Fatalf("file %+v", cfg.File)
	case !cfg.Repeats.Enabled, time.Duration(cfg.Repeats.Window) != 30*time.Second, time.Duration(cfg.NotifyTimeout) != 2*time.Second:
		t.Fatalf("durations %+v", cfg)
	case cfg.Telegram.Token != `abc"#: x`, cfg.Telegram.ChatID != "-100123", !cfg.Telegram.MessagesOnly:
		t.Fatalf("telegram %+v", cfg.Telegram)
	case cfg.Pushover.Priorities[slog.LevelError+4] != 2, cfg.Pushover.Level != slog.LevelError:
		t.Fatalf("pushover %+v", cfg.Pushover)
	case cfg.Email.Host != "smtp.example.com", len(cfg.Email.To) != 2:
		t.Fatalf("email %+v", cfg.Email)
	case !cfg.Format.Level, !cfg.Format.Program:
		t.Fatalf("defaults lost %+v", cfg.Format)
	}

	if _, err := ParseYAML([]byte("consol:\n  level: info\n")); err == nil {
		t.Fatal("expected error for unknown key")
	}
	if cfg, err := ParseYAML(nil); err != nil || !cfg.Console.Enabled {
		t.Fatalf("empty config: %v %+v", err, cfg)
	}
}

// TestLoadConfig checks that LoadConfig picks the parser by extension.
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	y := filepath.Join(dir, "rlog.yaml")
	j := filepath.Join(dir, "rlog.json")
	os.WriteFile(y, []byte("program: y\n"), 0o644)
	os.WriteFile(j, []byte(`{"program":"j","close_timeout":"1s"}`), 0o644)
	if cfg, err := LoadConfig(y); err != nil || cfg.Program != "y" {
		t.Fatal(cfg.Program, err)
	}
	if cfg, err := LoadConfig(j); err != nil || cfg.Program != "j" || time.Duration(cfg.CloseTimeout) != time.Second {
		t.Fatal(cfg.Program, err)
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("expected error")
	}
}

// TestFile checks that records reach the file sink.
func TestFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "app.log")
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.Format.Program = false
	cfg.File = FileSink{Enabled: true, Level: slog.LevelInfo, Path: path, Daily: true}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l.Slog().Info("to file")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "INFO  to file") {
		t.Fatalf("%q %v", b, err)
	}
}

// TestUntilMidnight checks the daily rotation delay.
func TestUntilMidnight(t *testing.T) {
	now := time.Date(2026, 3, 1, 23, 30, 0, 0, time.UTC)
	if d := untilMidnight(now, true); d != 30*time.Minute {
		t.Fatal(d)
	}
}

// TestRepeats checks that identical records within the window collapse
// into the first record plus one summary, that different attributes are
// not grouped, and that a summary is stamped with the time it is written.
func TestRepeats(t *testing.T) {
	const window = 400 * time.Millisecond
	var out syncBuffer
	cfg := DefaultConfig()
	cfg.Format.Time = TimeUnixMilli
	cfg.Format.Program = false
	cfg.Console.Writer = &out
	cfg.Repeats = RepeatConfig{Enabled: true, Window: Duration(window)}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	log := l.Slog().With(ComponentKey, "c")
	for range 5 {
		log.Warn("disk full", "dev", "sda")
	}
	log.Warn("disk full", "dev", "sdb")
	time.Sleep(window * 3 / 2)          // first window ends, summary emitted
	log.Warn("disk full", "dev", "sda") // still within the follow-up window
	l.Close()                           // flushes the second summary

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []string{
		"WARN  [c] disk full dev=sda",
		"WARN  [c] disk full dev=sdb",
		"WARN  [c] disk full dev=sda repeated=4",
		"WARN  [c] disk full dev=sda repeated=1",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %q", lines)
	}
	stamps := make([]int64, len(lines))
	for i := range want {
		stamp, rest, _ := strings.Cut(lines[i], " ")
		if rest != want[i] {
			t.Fatalf("line %d: got %q want %q", i, rest, want[i])
		}
		stamps[i], _ = strconv.ParseInt(stamp, 10, 64)
	}
	// Stamped when written, not when the last repeat happened, so the log
	// stays in time order.
	if d := stamps[2] - stamps[0]; d < window.Milliseconds()-50 {
		t.Fatalf("summary stamped %dms after the first record, want about %v", d, window)
	}
}

// TestPanickingErrorValue checks that an error value whose Error method
// panics (here on a nil pointer) is logged rather than crashing the caller.
func TestPanickingErrorValue(t *testing.T) {
	log, buf, _ := newBuf(t, func(c *Config) { c.Format = Format{Time: TimeNone} })
	var perr *os.PathError // (*PathError).Error dereferences its receiver
	log.Error("x", "err", perr)
	if got := buf.String(); got != "x err=<nil>\n" {
		t.Fatalf("got %q", got)
	}
}

// TestJSONKeepsUserLevelAttr checks that hiding the built-in level does not
// drop a caller's own attribute that happens to be called "level".
func TestJSONKeepsUserLevelAttr(t *testing.T) {
	log, buf, _ := newBuf(t, func(c *Config) {
		c.Console.JSON = true
		c.Format.Level = false
	})
	log.Info("x", "level", 3)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err, buf.String())
	}
	if m["level"] != 3.0 {
		t.Fatalf("got %v", m)
	}
}

// TestFileOpenError checks that New reports a log path that cannot be
// opened, instead of failing silently at the first write.
func TestFileOpenError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.File = FileSink{Enabled: true, Path: t.TempDir()} // a directory, not a file
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error")
	}
}

// capture is a fake HTTP notification service that records the forms it
// receives.
type capture struct {
	mu    sync.Mutex
	forms []url.Values
	paths []string
}

// server starts the fake service, answering every request with reply.
func (c *capture) server(t *testing.T, reply string) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		c.mu.Lock()
		c.forms = append(c.forms, r.PostForm)
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		w.Write([]byte(reply))
	}))
	t.Cleanup(s.Close)
	return s
}

// TestTelegramAndPushover checks record notifications end to end.
func TestTelegramAndPushover(t *testing.T) {
	var tg, po capture
	tgs := tg.server(t, `{"ok":true}`)
	pos := po.server(t, `{"status":1}`)
	var errBuf bytes.Buffer
	log, _, l := newBuf(t, func(c *Config) {
		c.ErrorWriter = &errBuf
		c.Telegram = TelegramSink{Enabled: true, Level: slog.LevelWarn, Token: "T", ChatID: "42", APIURL: tgs.URL}
		c.Pushover = PushoverSink{Enabled: true, Level: slog.LevelInfo, Token: "A", User: "U", APIURL: pos.URL}
	})
	log.With(ComponentKey, "bridge").Info("info msg")
	log.With(ComponentKey, "bridge").Error("down", "host", "h1")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if errBuf.Len() > 0 {
		t.Fatal(errBuf.String())
	}
	tg.mu.Lock()
	defer tg.mu.Unlock()
	po.mu.Lock()
	defer po.mu.Unlock()
	if len(tg.forms) != 1 || tg.paths[0] != "/botT/sendMessage" ||
		tg.forms[0].Get("chat_id") != "42" || tg.forms[0].Get("text") != "prog ERROR\n[bridge] down host=h1" {
		t.Fatalf("telegram %v %v", tg.paths, tg.forms)
	}
	if len(po.forms) != 2 || po.forms[0].Get("priority") != "-1" || po.forms[1].Get("priority") != "1" ||
		po.forms[1].Get("title") != "prog ERROR" || po.forms[1].Get("message") != "[bridge] down host=h1" {
		t.Fatalf("pushover %v", po.forms)
	}
	// Logging after Close must not panic.
	log.Error("after close")
}

// TestSpecialMessages checks Notify, NotifyRaw and Send: formatting, raw
// mode, HTML, targets, priorities and MessagesOnly sinks.
func TestSpecialMessages(t *testing.T) {
	var tg, po capture
	tgs := tg.server(t, `{"ok":true}`)
	pos := po.server(t, `{"status":1}`)
	var errBuf bytes.Buffer
	log, _, l := newBuf(t, func(c *Config) {
		c.ErrorWriter = &errBuf
		c.Telegram = TelegramSink{Enabled: true, Level: slog.LevelError, Token: "T", ChatID: "1", APIURL: tgs.URL, MessagesOnly: true}
		c.Pushover = PushoverSink{Enabled: true, Level: slog.LevelError, Token: "A", User: "U", APIURL: pos.URL}
	})
	log.Error("record") // pushover only: telegram is messages-only
	l.Notify("backup done", "files", 12, "took", "3m")
	l.NotifyRaw("", "just text")
	l.Send(Message{Title: "Report", Text: "<i>ok</i>", HTML: true, To: ToTelegram})
	l.Send(Message{Text: "wake up", Priority: 2, To: ToPushover})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if errBuf.Len() > 0 {
		t.Fatal(errBuf.String())
	}

	tg.mu.Lock()
	defer tg.mu.Unlock()
	texts := []string{}
	for _, f := range tg.forms {
		texts = append(texts, f.Get("text")+"|"+f.Get("parse_mode"))
	}
	wantTG := []string{
		"prog\nbackup done files=12 took=3m|",
		"just text|",
		"<b>prog: Report</b>\n<i>ok</i>|HTML",
	}
	if strings.Join(texts, ";") != strings.Join(wantTG, ";") {
		t.Fatalf("telegram got %q want %q", texts, wantTG)
	}

	po.mu.Lock()
	defer po.mu.Unlock()
	if len(po.forms) != 4 {
		t.Fatalf("pushover got %v", po.forms)
	}
	if f := po.forms[0]; f.Get("title") != "prog ERROR" || f.Get("message") != "record" {
		t.Fatalf("pushover record %v", f)
	}
	if f := po.forms[1]; f.Get("title") != "prog" || f.Get("priority") != "0" {
		t.Fatalf("pushover notify %v", f)
	}
	if f := po.forms[2]; f.Has("title") || f.Get("message") != "just text" {
		t.Fatalf("pushover raw %v", f)
	}
	if f := po.forms[3]; f.Get("priority") != "2" || f.Get("retry") != "60" {
		t.Fatalf("pushover emergency %v", f)
	}
}

// TestPackageNotifyBeforeInit checks that the package-level helpers are
// safe no-ops before Init.
func TestPackageNotifyBeforeInit(t *testing.T) {
	old := std.Swap(nil)
	defer std.Store(old)
	Notify("nothing")
	NotifyRaw("t", "x")
	Send(Message{Text: "x"})
	if err := Default().Close(); err != nil {
		t.Fatal(err)
	}
}

// TestNotifierErrorReported checks that a service error reaches ErrorWriter.
func TestNotifierErrorReported(t *testing.T) {
	var tg capture
	tgs := tg.server(t, `{"ok":false,"description":"chat not found"}`)
	var errBuf bytes.Buffer
	log, _, l := newBuf(t, func(c *Config) {
		c.ErrorWriter = &errBuf
		c.Telegram = TelegramSink{Enabled: true, Level: slog.LevelError, Token: "T", ChatID: "1", APIURL: tgs.URL}
	})
	log.Error("x")
	l.Close()
	if !strings.Contains(errBuf.String(), "chat not found") {
		t.Fatal(errBuf.String())
	}
}

// TestUnreachableNotifierThrottled checks that repeated failures against
// a dead endpoint are reported once plus a suppressed count, and that the
// bot token never appears in the output.
func TestUnreachableNotifierThrottled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close() // nothing listens there now

	var errBuf syncBuffer
	log, _, l := newBuf(t, func(c *Config) {
		c.ErrorWriter = &errBuf
		c.CloseTimeout = Duration(30 * time.Second) // refused connections can be slow on Windows
		c.Telegram = TelegramSink{Enabled: true, Level: slog.LevelError, Token: "SECRET", ChatID: "1", APIURL: dead}
	})
	for range 5 {
		log.Error("x")
	}
	l.Close()
	out := errBuf.String()
	if strings.Count(out, "\n") != 2 || !strings.Contains(out, "rlog: telegram: 4 further errors suppressed") {
		t.Fatalf("got %q", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("token leaked: %q", out)
	}
}

// TestCloseDoesNotHang checks that a notification service that never
// answers delays neither logging nor Close beyond CloseTimeout.
func TestCloseDoesNotHang(t *testing.T) {
	block := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer s.Close()
	defer close(block)

	var errBuf syncBuffer
	log, _, l := newBuf(t, func(c *Config) {
		c.ErrorWriter = &errBuf
		c.QueueSize = 2
		c.CloseTimeout = Duration(200 * time.Millisecond)
		c.Pushover = PushoverSink{Enabled: true, Level: slog.LevelInfo, Token: "A", User: "U", APIURL: s.URL}
	})
	start := time.Now()
	for range 50 { // far more than the queue holds
		log.Info("x")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("logging blocked for %v", d)
	}
	start = time.Now()
	err := l.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v", d)
	}
	if err == nil || !strings.Contains(err.Error(), "close timeout") {
		t.Fatalf("Close error %v", err)
	}
	if !strings.Contains(errBuf.String(), "queue full") {
		t.Fatalf("no drop reported: %q", errBuf.String())
	}
}

// TestEmail checks a record email end to end against a fake SMTP server.
func TestEmail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go fakeSMTP(ln, got)

	port := ln.Addr().(*net.TCPAddr).Port
	var errBuf bytes.Buffer
	log, _, l := newBuf(t, func(c *Config) {
		c.ErrorWriter = &errBuf
		c.Email = EmailSink{Enabled: true, Level: slog.LevelError, Host: "127.0.0.1", Port: port,
			Security: SecurityNone, From: "Bot <bot@example.com>", To: []string{"a@example.com", "b@example.com"}}
	})
	log.Error("disk full", "free", "0 B")
	l.Close()
	if errBuf.Len() > 0 {
		t.Fatal(errBuf.String())
	}
	msg := <-got
	for _, want := range []string{"RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>",
		"Subject: prog ERROR: disk full free=\"0 B\"", "\r\ndisk full free=\"0 B\"\r\n"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in:\n%s", want, msg)
		}
	}
}

// TestEmailMessageFormats checks subjects, content types and footers for
// special messages.
func TestEmailMessageFormats(t *testing.T) {
	s := &emailSender{cfg: EmailSink{From: "a@example.com", To: []string{"b@example.com"}}}
	l := &Logger{program: "prog"}
	build := func(m Message) string {
		ch := make(chan delivery, 1)
		l.outputs = []output{{target: ToEmail, deliver: func(d delivery) { ch <- d }}}
		l.Send(m)
		return string(s.message(<-ch))
	}
	cases := []struct {
		m       Message
		want    []string
		notWant []string
	}{
		{Message{Text: "line1\nline2"}, []string{"Subject: prog: line1\n", "text/plain", "  prog\n"}, nil},
		{Message{Title: "Daily", Text: "x"}, []string{"Subject: prog: Daily\n"}, nil},
		{Message{Title: "Raw subj", Text: "body", Raw: true}, []string{"Subject: Raw subj\n", "\n\nbody"}, []string{"prog"}},
		{Message{Text: "<p>hi</p>", HTML: true}, []string{"Subject: prog\n", "text/html", "<hr><small>"}, nil},
	}
	for _, c := range cases {
		out := build(c.m)
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%+v: missing %q in:\n%s", c.m, w, out)
			}
		}
		for _, w := range c.notWant {
			if strings.Contains(out, w) {
				t.Errorf("%+v: unexpected %q in:\n%s", c.m, w, out)
			}
		}
	}
}

// fakeSMTP accepts one connection, plays a permissive SMTP server and sends
// the whole conversation (commands and data) on got.
func fakeSMTP(ln net.Listener, got chan<- string) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	var all strings.Builder
	reply := func(s string) { conn.Write([]byte(s + "\r\n")) }
	reply("220 fake")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			break
		}
		all.WriteString(line)
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			reply("250 fake")
		case cmd == "DATA":
			reply("354 go")
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				all.WriteString(l)
			}
			reply("250 ok")
		case cmd == "QUIT":
			reply("221 bye")
			got <- all.String()
			return
		default:
			reply("250 ok")
		}
	}
	got <- all.String()
}

// TestPushoverPriority checks the level-to-priority lookup.
func TestPushoverPriority(t *testing.T) {
	s := &pushoverSender{cfg: PushoverSink{Priorities: map[slog.Level]int{slog.LevelWarn: 0, slog.LevelError + 4: 2}}}
	cases := map[slog.Level]int{slog.LevelInfo: 0, slog.LevelWarn: 0, slog.LevelError: 0, slog.LevelError + 4: 2, slog.LevelError + 8: 2}
	for l, want := range cases {
		if got := s.priority(l); got != want {
			t.Errorf("%s: got %d want %d", l, got, want)
		}
	}
}

// TestValidation checks that an incomplete notifier config is rejected.
func TestValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Telegram.Enabled = true
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error")
	}
}

// TestProgramDefaultsToExecutable checks that an empty Config.Program falls
// back to the executable's name in console lines, as documented.
func TestProgramDefaultsToExecutable(t *testing.T) {
	log, buf, _ := newBuf(t, func(c *Config) {
		c.Program = ""
		c.Format.Time = TimeNone
	})
	log.Info("x")
	exe := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if want := "INFO  " + exe + ": x\n"; buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}

// TestMessageOutputs checks where special messages go: by default to every
// sink including console and file (as NOTE lines), only to the sinks named
// in To when given, and never to a sink with notifications: false.
func TestMessageOutputs(t *testing.T) {
	var tg, po capture
	tgs := tg.server(t, `{"ok":true}`)
	pos := po.server(t, `{"status":1}`)
	path := filepath.Join(t.TempDir(), "app.log")
	off := false
	var errBuf bytes.Buffer
	_, buf, l := newBuf(t, func(c *Config) {
		c.ErrorWriter = &errBuf
		c.Format.Time = TimeNone
		c.Format.Source = SourceShort // messages must still get no source
		c.File = FileSink{Enabled: true, Level: slog.LevelError, Path: path, JSON: true}
		c.Telegram = TelegramSink{Enabled: true, Level: slog.LevelError, Token: "T", ChatID: "1", APIURL: tgs.URL}
		c.Pushover = PushoverSink{Enabled: true, Level: slog.LevelError, Token: "A", User: "U", APIURL: pos.URL,
			Notifications: &off}
	})
	l.Notify("all", "n", 1)                                                    // console, file, telegram (not pushover: off)
	l.NotifyRaw("Title", "line1\nline2")                                       // same sinks
	l.Send(Message{Text: "console only", To: ToConsole})                       // console only
	l.Send(Message{Text: "file+po", To: ToFile | ToPushover})                  // file only: pushover is off
	l.Send(Message{Title: "T", Text: "<b>tg</b>", HTML: true, To: ToTelegram}) // telegram only
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if errBuf.Len() > 0 {
		t.Fatal(errBuf.String())
	}

	wantConsole := "NOTE  prog: all n=1\nNOTE  prog: Title: line1 | line2\nNOTE  prog: console only\n"
	if buf.String() != wantConsole {
		t.Fatalf("console got %q want %q", buf.String(), wantConsole)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err, line)
		}
		if _, has := m["source"]; has {
			t.Fatalf("message has a source: %v", m)
		}
		if m["level"] != "NOTE" {
			t.Fatalf("file level %v", m)
		}
		msgs = append(msgs, fmt.Sprint(m["msg"], "|", m["title"]))
	}
	wantFile := []string{"all n=1|<nil>", "line1 | line2|Title", "file+po|<nil>"}
	if strings.Join(msgs, ";") != strings.Join(wantFile, ";") {
		t.Fatalf("file got %q want %q", msgs, wantFile)
	}

	tg.mu.Lock()
	defer tg.mu.Unlock()
	if len(tg.forms) != 3 {
		t.Fatalf("telegram got %d messages: %v", len(tg.forms), tg.forms)
	}
	po.mu.Lock()
	defer po.mu.Unlock()
	if len(po.forms) != 0 {
		t.Fatalf("pushover should get nothing, got %v", po.forms)
	}
}

// TestNotificationsYAML checks that notifications: false is read from YAML
// and that an absent key leaves the setting unset (meaning yes).
func TestNotificationsYAML(t *testing.T) {
	cfg, err := ParseYAML([]byte("file:\n  notifications: false\nconsole:\n  notifications: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if notificationsOn(cfg.File.Notifications) || !notificationsOn(cfg.Console.Notifications) ||
		!notificationsOn(cfg.Telegram.Notifications) {
		t.Fatalf("%v %v %v", cfg.File.Notifications, cfg.Console.Notifications, cfg.Telegram.Notifications)
	}
}
