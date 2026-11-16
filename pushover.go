package rlog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// PushoverSink sends log records and notifications through the Pushover API.
type PushoverSink struct {
	// Enabled turns the sink on.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Level is the minimum level of log records sent. Notifications ignore
	// it.
	Level slog.Level `json:"level" yaml:"level"`
	// MessagesOnly sends only notifications here, no log records.
	MessagesOnly bool `json:"messages_only" yaml:"messages_only"`
	// Notifications controls whether notifications (Notify, NotifyRaw,
	// Send) are sent here. Nil (not set) means yes.
	Notifications *bool `json:"notifications" yaml:"notifications"`
	// Token is the application API token.
	Token string `json:"token" yaml:"token"`
	// User is the user or group key.
	User string `json:"user" yaml:"user"`
	// Device optionally limits delivery to these device names (comma separated).
	Device string `json:"device" yaml:"device"`
	// Priorities maps a level to a Pushover priority (-2..2). A record uses
	// the entry for the highest level at or below its own. Nil means
	// DEBUG:-2 INFO:-1 WARN:0 ERROR:1. Priority 2 (emergency) repeats every
	// 60s for an hour until acknowledged. Notifications use
	// Message.Priority instead.
	Priorities map[slog.Level]int `json:"priorities" yaml:"priorities"`
	// APIURL overrides https://api.pushover.net/1/messages.json.
	APIURL string `json:"api_url" yaml:"api_url"`
}

// Pushover API limits on the title and message lengths.
const (
	pushoverMaxTitle   = 250
	pushoverMaxMessage = 1024
)

// defaultPushoverPriorities is used when PushoverSink.Priorities is nil.
var defaultPushoverPriorities = map[slog.Level]int{
	slog.LevelDebug: -2,
	slog.LevelInfo:  -1,
	slog.LevelWarn:  0,
	slog.LevelError: 1,
}

// pushoverSender is the prepared client for one PushoverSink.
type pushoverSender struct {
	cfg PushoverSink // validated, with defaults filled in
}

// sender validates the sink and builds its client.
func (c PushoverSink) sender() (sender, error) {
	if c.Token == "" || c.User == "" {
		return nil, errors.New("token and user are required")
	}
	if c.APIURL == "" {
		c.APIURL = "https://api.pushover.net/1/messages.json"
	}
	if c.Priorities == nil {
		c.Priorities = defaultPushoverPriorities
	}
	for l, p := range c.Priorities {
		if p < -2 || p > 2 {
			return nil, fmt.Errorf("priority %d for %s is outside -2..2", p, l)
		}
	}
	return &pushoverSender{cfg: c}, nil
}

// priority returns the priority for a record at level: the entry for the
// highest configured level at or below it, or 0 if none applies.
func (s *pushoverSender) priority(level slog.Level) int {
	best, found, prio := slog.Level(0), false, 0
	for l, p := range s.cfg.Priorities {
		if l <= level && (!found || l > best) {
			best, found, prio = l, true, p
		}
	}
	return prio
}

// send posts one message to the Pushover API.
func (s *pushoverSender) send(ctx context.Context, d delivery) error {
	prio := s.priority(d.Level)
	if d.note {
		prio = min(max(d.Priority, -2), 2)
	}
	title, msg := d.heading(), d.Text
	if msg == "" { // Pushover requires a message
		title, msg = "", title
	}
	if msg == "" {
		return errEmptyNotification
	}
	form := url.Values{
		"token":    {s.cfg.Token},
		"user":     {s.cfg.User},
		"message":  {truncate(msg, pushoverMaxMessage)},
		"priority": {strconv.Itoa(prio)},
	}
	if title != "" {
		form.Set("title", truncate(title, pushoverMaxTitle))
	}
	if d.HTML {
		form.Set("html", "1")
	}
	if !d.Time.IsZero() {
		form.Set("timestamp", strconv.FormatInt(d.Time.Unix(), 10))
	}
	if s.cfg.Device != "" {
		form.Set("device", s.cfg.Device)
	}
	if prio == 2 {
		form.Set("retry", "60")
		form.Set("expire", "3600")
	}
	var out struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	status, err := postForm(ctx, s.cfg.APIURL, form, &out)
	if err != nil {
		return err
	}
	if status != http.StatusOK || out.Status != 1 {
		return fmt.Errorf("HTTP %d: %s", status, strings.Join(out.Errors, "; "))
	}
	return nil
}
