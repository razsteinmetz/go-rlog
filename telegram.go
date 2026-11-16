package rlog

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// TelegramSink sends log records and notifications to a chat through the
// Telegram Bot API.
type TelegramSink struct {
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
	// Token is the bot token from @BotFather.
	Token string `json:"token" yaml:"token"`
	// ChatID is the user, group or channel id (or @channelname).
	ChatID string `json:"chat_id" yaml:"chat_id"`
	// APIURL overrides https://api.telegram.org (for tests or a local Bot API server).
	APIURL string `json:"api_url" yaml:"api_url"`
}

// telegramMaxText is the Bot API's limit on a message's length.
const telegramMaxText = 4096

// telegramSender is the prepared client for one TelegramSink.
type telegramSender struct {
	endpoint string // full sendMessage URL, including the token
	chatID   string
}

// sender validates the sink and builds its client.
func (c TelegramSink) sender() (sender, error) {
	if c.Token == "" || c.ChatID == "" {
		return nil, errors.New("token and chat_id are required")
	}
	base := c.APIURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	return &telegramSender{
		endpoint: strings.TrimRight(base, "/") + "/bot" + c.Token + "/sendMessage",
		chatID:   c.ChatID,
	}, nil
}

// text builds the message: the heading on the first line (bold for
// formatted HTML notifications), then the body.
func (s *telegramSender) text(d delivery) string {
	h := d.heading()
	if d.HTML && !d.Raw && h != "" {
		h = "<b>" + html.EscapeString(h) + "</b>"
	}
	if h == "" || d.Text == "" {
		return h + d.Text
	}
	return h + "\n" + d.Text
}

// send posts one message with sendMessage.
func (s *telegramSender) send(ctx context.Context, d delivery) error {
	text := s.text(d)
	if text == "" {
		return errEmptyNotification
	}
	form := url.Values{
		"chat_id":                  {s.chatID},
		"text":                     {truncate(text, telegramMaxText)},
		"disable_web_page_preview": {"true"},
	}
	if d.HTML {
		form.Set("parse_mode", "HTML")
	}
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	status, err := postForm(ctx, s.endpoint, form, &out)
	if err != nil {
		return err
	}
	if status != http.StatusOK || !out.OK {
		return fmt.Errorf("HTTP %d: %s", status, out.Description)
	}
	return nil
}
