package rlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Target selects the sinks a special message goes to. Values can be
// combined with |, e.g. ToTelegram|ToFile.
type Target uint8

// Message targets.
const (
	ToTelegram Target = 1 << iota // the Telegram sink
	ToPushover                    // the Pushover sink
	ToEmail                       // the email sink
	ToConsole                     // the console sink
	ToFile                        // the file sink
	ToAll      Target = 0         // every enabled sink that accepts notifications
)

// Message is a special, level-less notification: a report, a "started" or
// "backup finished" notice, anything that is not a log record.
//
// It goes to the sinks selected by To (all of them when To is ToAll),
// regardless of their Level, but only to sinks that are enabled and whose
// Notifications setting is not false. That setting always wins: a sink with
// notifications: false never receives messages, even when named in To.
// On the console and in the file a message is written as one NOTE line.
//
// A formatted message (Raw false) is decorated like a log notification: the
// title is "program" or "program: Title", and the email body ends with the
// time and program. A raw message is sent exactly as given: the title is
// Title (omitted when empty) and nothing is added to the text.
type Message struct {
	// Title is the heading: Telegram's first line, Pushover's title and the
	// email subject. See the type comment for how Raw changes it.
	Title string
	// Text is the body.
	Text string
	// Raw sends Title and Text without rlog's decorations.
	Raw bool
	// HTML marks Text (and, for raw messages, Title) as HTML: Telegram uses
	// parse_mode=HTML (which allows only <b>, <i>, <u>, <s>, <a>, <code>,
	// <pre> and a few more tags), Pushover sets html=1, and the email is
	// sent as text/html. Long texts are cut to each service's limit, which
	// can break markup.
	HTML bool
	// To selects the sinks; ToAll (zero) means every sink that accepts
	// notifications.
	To Target
	// Priority is the Pushover priority, -2..2 (0 is normal; 2 is an
	// emergency that repeats every 60s for an hour until acknowledged).
	// Other sinks ignore it.
	Priority int
}

// Send delivers m to the selected sinks and returns at once: console and
// file lines are written immediately, and Telegram, Pushover and email are
// queued, so Send never waits on the network. Delivery errors go to
// Config.ErrorWriter. Send does nothing on a nil Logger.
func (l *Logger) Send(m Message) {
	if l == nil || len(l.outputs) == 0 {
		return
	}
	n := notification{
		Program:  l.program,
		Time:     time.Now(),
		Text:     m.Text,
		Message:  true,
		Raw:      m.Raw,
		HTML:     m.HTML,
		Priority: m.Priority,
	}
	switch {
	case m.Raw:
		n.Title = m.Title
		n.Subject = m.Title
		if n.Subject == "" {
			n.Subject = firstLine(m.Text)
		}
	case m.Title != "":
		n.Title = l.program + ": " + m.Title
		n.Subject = n.Title
	default:
		n.Title = l.program
		if m.HTML {
			n.Subject = n.Title // the first line of HTML is markup, not a subject
		}
	}

	emit := func(n notification) {
		for _, o := range l.outputs {
			if m.To == ToAll || m.To&o.target != 0 {
				o.deliver(n, m.Title)
			}
		}
	}
	if l.repeats != nil {
		key := fmt.Sprintf("\x01msg\x00%d\x00%t\x00%t\x00%d\x00%s\x00%s", m.To, m.Raw, m.HTML, m.Priority, m.Title, m.Text)
		pass := l.repeats.allow(key, func(count int) {
			n2 := n
			n2.Time = time.Now()
			n2.Text += fmt.Sprintf("\n(repeated %d more times)", count)
			emit(n2)
		})
		if !pass {
			return
		}
	}
	emit(n)
}

// Notify sends a formatted special message. msg and the slog-style
// key/value args are rendered like a log record ("msg key=value ..."),
// and the title is the program name. See Message.
func (l *Logger) Notify(msg string, args ...any) {
	l.Send(Message{Text: formatArgs(msg, args...)})
}

// NotifyRaw sends title and text to every sink that accepts notifications,
// exactly as given. title may be empty. See Message.
func (l *Logger) NotifyRaw(title, text string) {
	l.Send(Message{Title: title, Text: text, Raw: true})
}

// Send calls Send on the Logger installed by Init. Before Init it does
// nothing.
func Send(m Message) { Default().Send(m) }

// Notify calls Notify on the Logger installed by Init. Before Init it does
// nothing.
func Notify(msg string, args ...any) { Default().Notify(msg, args...) }

// NotifyRaw calls NotifyRaw on the Logger installed by Init. Before Init it
// does nothing.
func NotifyRaw(title, text string) { Default().NotifyRaw(title, text) }

// notification is one queued item: either a log record in the short format
// shared by all notifiers, or a special Message.
type notification struct {
	Program  string     // Config.Program
	Level    slog.Level // record level (unused for messages)
	Time     time.Time  // record or send time
	Title    string     // message heading; records derive theirs, see heading
	Subject  string     // email subject override; "" derives it, see subject
	Text     string     // records: "[component] message key=value ..."
	Message  bool       // a special message rather than a log record
	Raw      bool       // message without decorations
	HTML     bool       // Text is HTML
	Priority int        // Pushover priority for messages
}

// heading is the title line: "program LEVEL" for records, the prepared
// Title for messages (possibly empty for raw messages).
func (n notification) heading() string {
	if n.Message {
		return n.Title
	}
	return n.Program + " " + n.Level.String()
}

// subject is the email subject: the override if set, otherwise
// "heading: first line of text".
func (n notification) subject() string {
	if n.Subject != "" {
		return n.Subject
	}
	return n.heading() + ": " + firstLine(n.Text)
}

// sender delivers one notification to one service.
type sender interface {
	// send delivers n, giving up when ctx ends.
	send(ctx context.Context, n notification) error
}

// output is one sink that accepts special messages, as seen by Logger.Send.
type output struct {
	target Target // which ToXxx bit selects it
	// deliver writes or queues n. title is the caller's own Message.Title,
	// which console and file lines use instead of the decorated n.Title.
	deliver func(n notification, title string)
}

// notificationsOn reports whether a sink's Notifications setting lets
// special messages through: unset (nil) means yes.
func notificationsOn(p *bool) bool { return p == nil || *p }

// levelMessage is the pseudo-level of the records that carry special
// messages to the console and file. It is printed as "NOTE". Those records
// are handed straight to the sink's handler, so the value never meets a
// level filter.
const levelMessage slog.Level = 1 << 30

// noteLabel is how levelMessage is printed.
const noteLabel = "NOTE"

// messageWriter returns an output deliver function that writes a special
// message to a console or file handler as one record at levelMessage.
// Newlines in the text are replaced so the message stays on one line. In
// text output the caller's title prefixes the message ("Title: text"); in
// JSON output it is a separate "title" key.
func messageWriter(h slog.Handler, json bool) func(notification, string) {
	return func(n notification, title string) {
		text := strings.NewReplacer("\r\n", " | ", "\n", " | ", "\r", " | ").Replace(n.Text)
		msg := text
		if title != "" && !json {
			msg = title + ": " + text
		}
		r := slog.NewRecord(n.Time, levelMessage, msg, 0)
		if title != "" && json {
			r.AddAttrs(slog.String("title", title))
		}
		_ = h.Handle(context.Background(), r)
	}
}

// dispatcher sends notifications from a queue on its own goroutine so that
// logging never waits on the network. A failed or panicking send is
// reported and the dispatcher moves on to the next notification.
type dispatcher struct {
	name    string        // sink name for error messages
	snd     sender        // the service client
	timeout time.Duration // bound on one send
	report  *errReporter  // throttled error output

	ctx    context.Context    // canceled when Close gives up
	cancel context.CancelFunc // cancels ctx

	mu     sync.RWMutex      // guards closed against enqueue
	closed bool              // set by close; later enqueues are dropped
	ch     chan notification // the queue
	done   chan struct{}     // closed when run returns
}

// newDispatcher starts a dispatcher with a queue of size entries.
func newDispatcher(name string, s sender, size int, timeout time.Duration, errw io.Writer) *dispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	d := &dispatcher{
		name:    name,
		snd:     s,
		timeout: timeout,
		report:  &errReporter{w: errw, every: errReportInterval},
		ctx:     ctx,
		cancel:  cancel,
		ch:      make(chan notification, size),
		done:    make(chan struct{}),
	}
	go d.run()
	return d
}

// enqueue queues n without blocking. When the queue is full, n is dropped
// and the drop is reported. After close it does nothing.
func (d *dispatcher) enqueue(n notification) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return
	}
	select {
	case d.ch <- n:
	default:
		d.report.printf("%s: queue full, dropped: %s", d.name, truncate(firstLine(n.Text), 200))
	}
}

// run delivers queued notifications until the queue is closed. Once close
// has given up (ctx canceled), the remainder is discarded unsent.
func (d *dispatcher) run() {
	defer close(d.done)
	for n := range d.ch {
		if d.ctx.Err() != nil {
			continue
		}
		d.deliver(n)
	}
}

// deliver sends one notification with a timeout, reporting failures and
// recovering from panics in the sender so the dispatcher keeps running.
func (d *dispatcher) deliver(n notification) {
	defer func() {
		if r := recover(); r != nil {
			d.report.printf("%s: panic while sending: %v", d.name, r)
		}
	}()
	ctx, cancel := context.WithTimeout(d.ctx, d.timeout)
	defer cancel()
	if err := d.snd.send(ctx, n); err != nil && d.ctx.Err() == nil {
		d.report.printf("%s: %v", d.name, err)
	}
}

// close stops accepting notifications and waits until the queue is drained
// or deadline passes. On timeout it aborts the send in progress, discards
// the rest and returns an error.
func (d *dispatcher) close(deadline time.Time) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	close(d.ch)
	d.mu.Unlock()

	defer d.report.flush()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-d.done:
		d.cancel()
		return nil
	case <-t.C:
		pending := len(d.ch) + 1 // the queue plus the one being sent
		d.cancel()
		return fmt.Errorf("rlog: %s: close timeout, dropped up to %d queued notifications", d.name, pending)
	}
}

// errReportInterval is the minimum time between two error lines from the
// same notifier; errors in between are counted and summarized.
const errReportInterval = time.Minute

// errReporter writes rlog's own errors to a writer, throttled so that a
// dead network or a bad API key cannot flood stderr.
type errReporter struct {
	w     io.Writer     // destination (Config.ErrorWriter)
	every time.Duration // minimum spacing between lines

	mu         sync.Mutex
	last       time.Time // when the last line was written
	suppressed int       // errors swallowed since then
}

// printf writes "rlog: <message>" unless another line was written within
// the interval, in which case the error is only counted.
func (e *errReporter) printf(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	if !e.last.IsZero() && now.Sub(e.last) < e.every {
		e.suppressed++
		return
	}
	msg := fmt.Sprintf(format, args...)
	if e.suppressed > 0 {
		msg += fmt.Sprintf(" (%d earlier errors suppressed)", e.suppressed)
		e.suppressed = 0
	}
	e.last = now
	fmt.Fprintf(e.w, "rlog: %s\n", msg)
}

// flush reports how many errors are still being held back, if any.
func (e *errReporter) flush() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.suppressed > 0 {
		fmt.Fprintf(e.w, "rlog: %d further notifier errors suppressed\n", e.suppressed)
		e.suppressed = 0
	}
}

// newNotifyHandler returns a handler that renders records in the short
// format and queues them on d.
func newNotifyHandler(d *dispatcher, level slog.Level, f Format, program string) slog.Handler {
	return &textHandler{
		level:  level,
		format: f,
		full:   false,
		emit: func(l slog.Level, t time.Time, text string) error {
			d.enqueue(notification{Program: program, Level: l, Time: t, Text: text})
			return nil
		},
	}
}

// httpClient is shared by the HTTP-based notifiers. Each request also
// carries the dispatcher's per-send deadline; this timeout is a backstop.
var httpClient = &http.Client{Timeout: time.Minute}

// truncate shortens s to at most max runes, marking the cut with "…".
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// firstLine returns s up to its first line break.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// errEmptyNotification is returned when there is nothing to send.
var errEmptyNotification = errors.New("empty notification")
