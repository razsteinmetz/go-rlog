package rlog

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Email security modes for EmailSink.Security.
const (
	SecurityStartTLS = "starttls" // plain connect, then STARTTLS (required); default port 587
	SecurityTLS      = "tls"      // implicit TLS; default port 465
	SecurityNone     = "none"     // no encryption; default port 25
)

// EmailSink sends each log record or notification as an email over SMTP.
type EmailSink struct {
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
	// Host is the SMTP server name; it is also checked against the TLS
	// certificate.
	Host string `json:"host" yaml:"host"`
	// Port is the SMTP port; 0 picks the default for Security.
	Port int `json:"port" yaml:"port"`
	// Security is "starttls" (default), "tls" or "none".
	Security string `json:"security" yaml:"security"`
	// Username enables PLAIN authentication; empty skips authentication.
	Username string `json:"username" yaml:"username"`
	// Password is the SMTP password.
	Password string `json:"password" yaml:"password"`
	// From is the sender, e.g. "Lutron <bot@example.com>".
	From string `json:"from" yaml:"from"`
	// To lists the recipients.
	To []string `json:"to" yaml:"to"`
}

// emailSender is the prepared client for one EmailSink.
type emailSender struct {
	cfg  EmailSink // validated, with Security and Port filled in
	from string    // bare envelope sender address
	to   []string  // bare envelope recipient addresses
}

// sender validates the sink and builds its client.
func (c EmailSink) sender() (sender, error) {
	if c.Host == "" || c.From == "" || len(c.To) == 0 {
		return nil, errors.New("host, from and to are required")
	}
	switch c.Security {
	case "":
		c.Security = SecurityStartTLS
	case SecurityStartTLS, SecurityTLS, SecurityNone:
	default:
		return nil, fmt.Errorf("unknown security %q", c.Security)
	}
	if c.Port == 0 {
		c.Port = map[string]int{SecurityStartTLS: 587, SecurityTLS: 465, SecurityNone: 25}[c.Security]
	}
	from, err := mail.ParseAddress(c.From)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	s := &emailSender{cfg: c, from: from.Address}
	for _, t := range c.To {
		a, err := mail.ParseAddress(t)
		if err != nil {
			return nil, fmt.Errorf("to %q: %w", t, err)
		}
		s.to = append(s.to, a.Address)
	}
	return s, nil
}

// send delivers one email. The whole SMTP conversation is bounded by ctx:
// its deadline is set on the connection, and canceling it (when Close
// gives up) aborts the connection.
func (s *emailSender) send(ctx context.Context, d delivery) error {
	c := s.cfg
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	tlsCfg := &tls.Config{ServerName: c.Host}
	dialer := &net.Dialer{}

	var conn net.Conn
	var err error
	if c.Security == SecurityTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()

	if c.Security == SecurityStartTLS {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("server does not offer STARTTLS")
		}
		if err := cl.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if c.Username != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := cl.Mail(s.from); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	for _, t := range s.to {
		if err := cl.Rcpt(t); err != nil {
			return fmt.Errorf("rcpt %s: %w", t, err)
		}
	}
	w, err := cl.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(s.message(d)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("data: %w", err)
	}
	return cl.Quit()
}

// message builds the RFC 5322 message. The SMTP data writer converts "\n"
// to CRLF and dot-stuffs lines. Records and formatted notifications end
// with a footer giving the time and program (and the level, for records);
// raw notifications do not.
func (s *emailSender) message(d delivery) []byte {
	subject := truncate(d.subject(), 150)
	t := d.Time
	if t.IsZero() {
		t = time.Now()
	}
	contentType := "text/plain"
	if d.HTML {
		contentType = "text/html"
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\n", s.cfg.From)
	fmt.Fprintf(&b, "To: %s\n", strings.Join(s.cfg.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\n", t.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\n")
	fmt.Fprintf(&b, "Content-Type: %s; charset=utf-8\n", contentType)
	b.WriteString("Content-Transfer-Encoding: 8bit\n\n")
	b.WriteString(d.Text)
	if !d.Raw {
		footer := t.Format(time.RFC3339) + "  " + d.Program
		if !d.note {
			footer += "  " + d.Level.String()
		}
		if d.HTML {
			fmt.Fprintf(&b, "\n<hr><small>%s</small>\n", html.EscapeString(footer))
		} else {
			fmt.Fprintf(&b, "\n\n%s\n", footer)
		}
	}
	return b.Bytes()
}
