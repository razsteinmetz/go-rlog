package rlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// TelegramSink sends records and special messages to a chat through the
// Telegram Bot API.
type TelegramSink struct {
	// Enabled turns the sink on.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Level is the minimum level of log records sent. Special messages
	// (Notify, Send) are sent regardless.
	Level slog.Level `json:"level" yaml:"level"`
	// MessagesOnly sends only special messages, no log records.
	MessagesOnly bool `json:"messages_only" yaml:"messages_only"`
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
// formatted HTML messages), then the body.
func (s *telegramSender) text(n notification) string {
	h := n.heading()
	if n.HTML && !n.Raw && h != "" {
		h = "<b>" + html.EscapeString(h) + "</b>"
	}
	switch {
	case h == "":
		return n.Text
	case n.Text == "":
		return h
	}
	return h + "\n" + n.Text
}

// send posts one message with sendMessage.
func (s *telegramSender) send(ctx context.Context, n notification) error {
	text := s.text(n)
	if text == "" {
		return errEmptyNotification
	}
	form := url.Values{
		"chat_id":                  {s.chatID},
		"text":                     {truncate(text, telegramMaxText)},
		"disable_web_page_preview": {"true"},
	}
	if n.HTML {
		form.Set("parse_mode", "HTML")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return redactURLError(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", redactURLError(err))
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || !out.OK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, out.Description)
	}
	return nil
}

// redactURLError strips the request URL, which contains the bot token,
// from a *url.Error so the token never reaches the error output.
func redactURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}
