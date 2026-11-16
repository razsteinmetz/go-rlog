package rlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Target selects the sinks a notification goes to. Values can be combined
// with |, e.g. ToTelegram|ToFile.
type Target uint8

// Notification targets.
const (
	ToTelegram Target = 1 << iota // the Telegram sink
	ToPushover                    // the Pushover sink
	ToEmail                       // the email sink
	ToConsole                     // the console sink
	ToFile                        // the file sink
	ToAll      Target = 0         // every enabled sink that accepts notifications
)

// Message is a notification: a level-less message such as a report or a
// "backup finished" notice, as opposed to a log record.
//
// It goes to the sinks selected by To (all of them when To is ToAll),
// regardless of their Level, but only to sinks that are enabled and whose
// Notifications setting is not false. That setting always wins: a sink with
// notifications: false never receives notifications, even when named in To.
// On the console and in the file a notification is written as one NOTE line.
//
// A formatted notification (Raw false) is decorated like a log record sent
// to a service: the title is "program" or "program: Title", and the email
// body ends with the time and program. A raw one is sent exactly as given:
// the title is Title (omitted when empty) and nothing is added to the text.
type Message struct {
	// Title is the heading: Telegram's first line, Pushover's title and the
	// email subject. See the type comment for how Raw changes it.
	Title string
	// Text is the body.
	Text string
	// Raw sends Title and Text without rlog's decorations.
	Raw bool
	// HTML marks Text (and, for raw notifications, Title) as HTML: Telegram
	// uses parse_mode=HTML (which allows only <b>, <i>, <u>, <s>, <a>,
	// <code>, <pre> and a few more tags), Pushover sets html=1, and the
	// email is sent as text/html. Long texts are cut to each service's
	// limit, which can break markup.
	HTML bool
	// To selects the sinks; ToAll (zero) means every sink that accepts
	// notifications.
	To Target
	// Priority is the Pushover priority, -2..2 (0 is normal; 2 is an
	// emergency that repeats every 60s for an hour until acknowledged).
	// Other sinks ignore it.
	Priority int
}

// Send delivers the notification m to the selected sinks and returns at
// once: console and file lines are written immediately, and Telegram,
// Pushover and email are queued, so Send never waits on the network.
// Delivery errors go to Config.ErrorWriter. Send does nothing on a nil
// Logger.
func (l *Logger) Send(m Message) {
	if l == nil || len(l.outputs) == 0 {
		return
	}
	emit := func(m Message) {
		d := delivery{Message: m, note: true, Program: l.program, Time: time.Now()}
		for _, o := range l.outputs {
			if m.To == ToAll || m.To&o.target != 0 {
				o.deliver(d)
			}
		}
	}
	if l.repeats != nil {
		// %#v quotes the strings, so different messages cannot share a key,
		// and the leading NUL keeps the key apart from those of records.
		pass := l.repeats.allow(fmt.Sprintf("\x00%#v", m), func(count int) {
			summary := m
			summary.Text += fmt.Sprintf("\n(repeated %d more times)", count)
			emit(summary)
		})
		if !pass {
			return
		}
	}
	emit(m)
}

// Notify sends a formatted notification. msg and the slog-style key/value
// args are rendered like a log record ("msg key=value ..."), and the title
// is the program name. See Message.
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

// delivery is one item handed to a sink: a notification, or a log record in
// the short format shared by the services (Telegram, Pushover, email).
type delivery struct {
	Message            // the notification; a log record sets only Text
	note    bool       // a notification rather than a log record
	Program string     // resolved Config.Program
	Level   slog.Level // record level (unused for notifications)
	Time    time.Time  // record time, or when the notification was sent
}

// heading is the title line: "program LEVEL" for a record; for a
// notification "program" or "program: Title", or just Title (possibly
// empty) when it is raw.
func (d delivery) heading() string {
	switch {
	case !d.note:
		return d.Program + " " + d.Level.String()
	case d.Raw:
		return d.Title
	case d.Title != "":
		return d.Program + ": " + d.Title
	}
	return d.Program
}

// subject is the email subject: the heading, followed by the first line of
// the text when the caller gave no title of their own. HTML text is never
// quoted in the subject, and a raw notification without a title uses the
// first line alone.
func (d delivery) subject() string {
	h := d.heading()
	switch {
	case d.Raw && h == "":
		return firstLine(d.Text)
	case d.Raw || d.Title != "" || d.HTML:
		return h
	}
	return h + ": " + firstLine(d.Text)
}

// sender delivers one item to one service.
type sender interface {
	// send delivers d, giving up when ctx ends.
	send(ctx context.Context, d delivery) error
}

// output is one sink that accepts notifications, as seen by Logger.Send.
type output struct {
	target  Target         // which ToXxx bit selects it
	deliver func(delivery) // writes or queues the notification
}

// notificationsOn reports whether a sink's Notifications setting lets
// notifications through: unset (nil) means yes.
func notificationsOn(p *bool) bool { return p == nil || *p }

// levelNote is the pseudo-level of the records that carry notifications to
// the console and file. It is printed as "NOTE". Those records are handed
// straight to the sink's handler, so the value never meets a level filter.
const levelNote slog.Level = 1 << 30

// noteLabel is how levelNote is printed.
const noteLabel = "NOTE"

// oneLine replaces line breaks so a notification stays on one log line.
var oneLine = strings.NewReplacer("\r\n", " | ", "\n", " | ", "\r", " | ")

// noteWriter returns an output deliver function that writes a notification
// to a console or file handler as one record at levelNote. In text output
// the title prefixes the text ("Title: text"); in JSON output it is a
// separate "title" key.
func noteWriter(h slog.Handler, asJSON bool) func(delivery) {
	return func(d delivery) {
		r := slog.NewRecord(d.Time, levelNote, oneLine.Replace(d.Text), 0)
		switch {
		case d.Title == "":
		case asJSON:
			r.AddAttrs(slog.String("title", d.Title))
		default:
			r.Message = d.Title + ": " + r.Message
		}
		_ = h.Handle(context.Background(), r)
	}
}

// dispatcher sends deliveries from a queue on its own goroutine so that
// logging never waits on the network. A failed or panicking send is
// reported and the dispatcher moves on to the next delivery.
type dispatcher struct {
	name    string        // sink name for error messages
	snd     sender        // the service client
	timeout time.Duration // bound on one send
	report  *errReporter  // throttled error output

	ctx    context.Context    // canceled when Close gives up
	cancel context.CancelFunc // cancels ctx

	mu     sync.RWMutex  // guards closed against enqueue
	closed bool          // set by close; later enqueues are dropped
	ch     chan delivery // the queue
	done   chan struct{} // closed when run returns
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
		ch:      make(chan delivery, size),
		done:    make(chan struct{}),
	}
	go d.run()
	return d
}

// enqueue queues v without blocking. When the queue is full, v is dropped
// and the drop is reported. After close it does nothing.
func (d *dispatcher) enqueue(v delivery) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return
	}
	select {
	case d.ch <- v:
	default:
		d.report.printf("%s: queue full, dropped: %s", d.name, truncate(firstLine(v.Text), 200))
	}
}

// run sends queued deliveries until the queue is closed. Once close has
// given up (ctx canceled), the remainder is discarded unsent.
func (d *dispatcher) run() {
	defer close(d.done)
	for v := range d.ch {
		if d.ctx.Err() == nil {
			d.deliver(v)
		}
	}
}

// deliver sends one delivery with a timeout, reporting failures and
// recovering from panics in the sender so the dispatcher keeps running.
func (d *dispatcher) deliver(v delivery) {
	defer func() {
		if r := recover(); r != nil {
			d.report.printf("%s: panic while sending: %v", d.name, r)
		}
	}()
	ctx, cancel := context.WithTimeout(d.ctx, d.timeout)
	defer cancel()
	if err := d.snd.send(ctx, v); err != nil && d.ctx.Err() == nil {
		d.report.printf("%s: %v", d.name, err)
	}
}

// close stops accepting deliveries and waits until the queue is drained
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

	defer d.report.flush(d.name)
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
// same service; errors in between are counted and summarized.
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

// flush reports how many errors of the named service are still being held
// back, if any.
func (e *errReporter) flush(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.suppressed > 0 {
		fmt.Fprintf(e.w, "rlog: %s: %d further errors suppressed\n", name, e.suppressed)
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
			d.enqueue(delivery{Message: Message{Text: text}, Program: program, Level: l, Time: t})
			return nil
		},
	}
}

// httpClient is shared by the HTTP-based services. Each request also
// carries the dispatcher's per-send deadline; this timeout is a backstop.
var httpClient = &http.Client{Timeout: time.Minute}

// postForm posts form to endpoint, decodes the JSON reply into out and
// returns the HTTP status. A reply that is not JSON leaves out untouched,
// so the caller's status check reports it.
func postForm(ctx context.Context, endpoint string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, fmt.Errorf("send: %w", withoutURL(err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(out)
	return resp.StatusCode, nil
}

// withoutURL strips the request URL from a *url.Error. Telegram's URL
// contains the bot token, which must never reach the error output.
func withoutURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// truncate shortens s to at most limit runes, marking the cut with "…".
func truncate(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit-1]) + "…"
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
