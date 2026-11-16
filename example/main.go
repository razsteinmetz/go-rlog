// Command example shows how to set up rlog from a YAML file and send the
// various kinds of log records and notifications.
//
// Run it from this folder:
//
//	cd example
//	go run .
//
// Secrets for Telegram, Pushover and email are read from .var (copy
// .var.example and fill it in). Services without secrets are skipped.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	rlog "github.com/razsteinmetz/go-rlog"
)

func main() {
	// 1. Put the secrets from .var into the environment, so that the
	//    ${NAME} references in rlog.yaml pick them up.
	if err := loadVars(".var"); err != nil {
		fmt.Println("note:", err)
	}

	// 2. Read the config and turn off the services that have no secrets.
	cfg, err := rlog.LoadConfig("rlog.yaml")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	skipUnconfigured(&cfg)

	// 3. Start the logger. Init also makes it the default for slog.
	l, err := rlog.Init(cfg)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	// Close sends whatever notifications are still queued.
	defer func() {
		if err := l.Close(); err != nil {
			fmt.Println(err)
		}
	}()

	logRecords()
	logWithComponents()
	logRepeats()
	sendMessages(l)
}

// logRecords shows plain log calls at each level, with key/value pairs.
func logRecords() {
	slog.Debug("debug details", "step", 1)
	slog.Info("example started", "pid", os.Getpid())
	slog.Warn("temperature high", "celsius", 31.5)
	slog.Error("sensor read failed", "sensor", "attic", "err", errors.New("timeout"))

	// Formatted text works too.
	slog.Info(fmt.Sprintf("uptime %s", 3*time.Hour+12*time.Minute))

	// Groups nest values: shown as reading.temp=21.5 reading.hum=40
	slog.Info("reading", slog.Group("reading", "temp", 21.5, "hum", 40))
}

// logWithComponents shows component loggers, shown as [name], and loggers
// that carry fixed values.
func logWithComponents() {
	garage := rlog.Component("garage")
	garage.Info("door opened")
	garage.Warn("door left open", "minutes", 15)

	// With adds values to every record of the returned logger.
	door := garage.With("door", "left")
	door.Info("door closed")
}

// logRepeats logs the same warning many times. With repeats enabled, the
// first one is printed at once and the rest are summarized as repeated=N
// when the window (2s in rlog.yaml) ends; Close prints a pending summary.
func logRepeats() {
	mqtt := rlog.Component("mqtt")
	for range 20 {
		mqtt.Warn("broker unreachable", "host", "192.168.1.10")
	}
}

// sendMessages shows special messages: notifications that are not log
// records. They go only to Telegram, Pushover and email, regardless of
// those sinks' levels, and are not written to the console or file.
func sendMessages(l *rlog.Logger) {
	// Formatted: the title is the program name, text "backup done files=1200".
	l.Notify("backup done", "files", 1200, "took", "4m10s")

	// Raw: sent exactly as given.
	l.NotifyRaw("Good morning", "Coffee machine is on.")

	// Full control: HTML text, only some services, Pushover priority.
	l.Send(rlog.Message{
		Title: "Daily report",
		Text:  "<b>3</b> warnings, <i>0</i> errors",
		HTML:  true,
		To:    rlog.ToTelegram | rlog.ToEmail,
	})
	l.Send(rlog.Message{
		Text:     "Water leak in the basement!",
		To:       rlog.ToPushover,
		Priority: 1, // high priority
	})

	// After Init, the package-level helpers do the same without l.
	rlog.Notify("example finished")
}

// skipUnconfigured disables each notification service whose secrets are
// empty, so the example runs with any subset of services configured.
func skipUnconfigured(cfg *rlog.Config) {
	if cfg.Telegram.Token == "" || cfg.Telegram.ChatID == "" {
		cfg.Telegram.Enabled = false
		fmt.Println("note: telegram skipped (TELEGRAM_TOKEN / TELEGRAM_CHAT_ID not set)")
	}
	if cfg.Pushover.Token == "" || cfg.Pushover.User == "" {
		cfg.Pushover.Enabled = false
		fmt.Println("note: pushover skipped (PUSHOVER_TOKEN / PUSHOVER_USER not set)")
	}
	if cfg.Email.Host == "" || cfg.Email.From == "" || len(cfg.Email.To) == 0 || cfg.Email.To[0] == "" {
		cfg.Email.Enabled = false
		fmt.Println("note: email skipped (SMTP_HOST / EMAIL_FROM / EMAIL_TO not set)")
	}
}

// loadVars reads KEY=VALUE lines from path into the environment. Blank
// lines and # comments are skipped, and surrounding quotes are removed.
// Variables already set in the environment are left alone.
func loadVars(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no secrets loaded: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}
