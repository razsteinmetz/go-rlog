# go-rlog

A `log/slog` handler that sends records to several sinks. Programs log with plain `slog` calls, and rlog supplies the handler.

| Sink     | What it does |
|----------|--------------|
| Console  | Text (optionally colored) or JSON, written to stderr (or any `io.Writer`) |
| File     | Text or JSON, rotated by [lumberjack](https://github.com/natefinch/lumberjack) daily and at 100 MB by default; keeps N files, gzips old ones |
| Telegram | Bot API `sendMessage` to one chat |
| Pushover | Priority set per level (emergency priority supported) |
| Email    | SMTP over STARTTLS, implicit TLS or plain, to one or more recipients |

Each sink has its own `Enabled` switch and minimum `Level`. Besides log records, rlog sends level-less **special messages** such as reports or "backup finished" notices, to every sink by default or to the ones you pick; each sink's `notifications` setting decides whether it accepts them.

## Usage

```go
import (
    "log/slog"
    rlog "github.com/razsteinmetz/go-rlog"
)

func main() {
    l, err := rlog.InitFile("rlog.yaml") // or rlog.Init(cfg); both install it as slog.Default
    if err != nil {
        panic(err)
    }
    defer l.Close() // flushes grouped repeats and sends queued notifications (bounded by close_timeout)

    slog.Info("starting", "version", version)
    log := rlog.Component("bridge") // = slog.Default().With("component", "bridge")
    log.Error("connection lost", "host", host, "err", err)

    rlog.Notify("backup finished", "files", 1200, "took", took) // special message
}
```

Or in code:

```go
cfg := rlog.DefaultConfig()
cfg.Program = "lutron"
cfg.Format.Time = rlog.TimeShort
cfg.Format.Source = rlog.SourceShort
cfg.File.Enabled = true
cfg.File.Path = "logs/lutron.log"
cfg.Repeats.Enabled = true
cfg.Telegram = rlog.TelegramSink{Enabled: true, Level: slog.LevelError,
    Token: os.Getenv("TG_TOKEN"), ChatID: os.Getenv("TG_CHAT")}
l, err := rlog.Init(cfg)
```

Text output:

```
2026-11-16 10:04:05.123 INFO  lutron: starting version=1.2.0 (main.go:24)
2026-11-16 10:04:07.940 ERROR lutron: [bridge] connection lost host=10.0.0.5 err="i/o timeout" (bridge.go:88)
```

Telegram, Pushover and email use a short format: the title is `lutron ERROR` and the body is `[bridge] connection lost host=10.0.0.5 err="i/o timeout"`.

## Configuration file

`LoadConfig(path)` / `InitFile(path)` read YAML, or JSON when the file ends in `.json`. The file is applied on top of `DefaultConfig()`, so only what differs needs to be given. Unknown keys are an error. Levels are strings (`debug`, `WARN`, `ERROR+4`), durations are strings (`30s`, `1m`). In any string value, `${NAME}` and `${NAME:-default}` are replaced from the environment, so secrets can stay out of the file.

```yaml
program: lutron

format:
  time: full          # full | datetime | short | time | timems | date | rfc3339 | rfc3339nano | iso8601 | unix | unixms | none | a Go layout
  utc: false
  source: short       # "" | short | full
  level: true
  component: true
  program: true

console:
  enabled: true
  level: info
  json: false
  color: auto         # never | auto | always
  notifications: true # special messages appear as NOTE lines (default true)

file:
  enabled: true
  level: info
  json: false
  path: logs/lutron.log
  notifications: false # keep special messages out of the log file
  max_size_mb: 100    # default 100
  daily: true         # default true
  max_backups: 7
  max_age_days: 0
  compress: true

repeats:              # group identical records/messages sent close together
  enabled: true
  window: 1m

queue_size: 100       # per notifier; extra notifications are dropped, never block
notify_timeout: 15s   # bound on one delivery attempt
close_timeout: 5s     # total wait in Close for queued notifications

telegram:
  enabled: true
  level: error
  token: ${TG_TOKEN}
  chat_id: "-100123456"

pushover:
  enabled: true
  level: warn
  messages_only: false   # true: special messages only, no log records
  notifications: true    # false: never send special messages here
  token: ${PO_TOKEN}
  user: ${PO_USER}
  priorities: {WARN: 0, ERROR: 1, ERROR+4: 2}

email:
  enabled: true
  level: error
  messages_only: true
  host: smtp.gmail.com
  security: starttls     # starttls | tls | none
  username: bot@example.com
  password: ${SMTP_PASSWORD}
  from: "Lutron <bot@example.com>"
  to: [me@example.com]
```

## Format options (`Config.Format`)

| Field       | Meaning |
|-------------|---------|
| `Time`      | A preset or a Go layout (see below). Empty means `full` |
| `UTC`       | Use UTC instead of local time (this also applies to daily rotation and backup names) |
| `Source`    | `""` (off), `short` (`file.go:42`) or `full` (full path) |
| `Level`     | Show the level |
| `Component` | Show the `component` attribute as `[name]` (in JSON, keep or drop the key) |
| `Program`   | Prefix lines with `Config.Program` (in JSON, add a `program` key) |

| Time preset   | Example |
|---------------|---------|
| `full`        | `2026-11-16 10:04:05.123` |
| `datetime`    | `2026-11-16 10:04:05` |
| `short`       | `11-16 10:04:05` |
| `time`        | `10:04:05` |
| `timems`      | `10:04:05.123` |
| `date`        | `2026-11-16` |
| `rfc3339`     | `2026-11-16T10:04:05+02:00` |
| `rfc3339nano` | `2026-11-16T10:04:05.123456789+02:00` |
| `iso8601`     | `2026-11-16T10:04:05.123+02:00` |
| `unix`, `unixms` | `1794816245` (a number in JSON output) |
| `none`        | no time |

Console colors (`console.color`): `never`, `auto` (only on a terminal, and not when `NO_COLOR` is set or `TERM=dumb`) or `always`. On Windows, rlog switches the console into ANSI mode. JSON output is never colored.

## Special messages

Special messages are not log records, so they ignore the sinks' `level`. Where they go:

- `Message.To` empty (the default, and always for `Notify`/`NotifyRaw`): **every enabled sink** whose `notifications` setting is not `false` — console, file, Telegram, Pushover and email.
- `Message.To` set (`rlog.ToConsole`, `ToFile`, `ToTelegram`, `ToPushover`, `ToEmail`, combined with `|`): only those sinks.
- A sink with `notifications: false` never gets special messages, even when named in `To`. Leaving the key out means `true`.
- A notification sink with `messages_only: true` gets only special messages, no log records.

On the console and in the file a message is one `NOTE` line (newlines become ` | `):

```
2026-11-16 12:30:00.000 NOTE  lutron: backup finished files=1200
2026-11-16 12:30:00.000 NOTE  lutron: Server up: All good.
```

In JSON output, `level` is `"NOTE"` and a title is a separate `title` key.

```go
l.Notify("backup finished", "files", 1200)   // formatted: title "lutron", text "backup finished files=1200"
l.NotifyRaw("Server up", "All good.")         // sent exactly as given (title may be "")
l.Send(rlog.Message{                          // full control
    Title:    "Daily report",                 // formatted title: "lutron: Daily report"
    Text:     "<b>3</b> errors today",
    HTML:     true,                           // Telegram parse_mode=HTML, Pushover html=1, email text/html
    To:       rlog.ToTelegram | rlog.ToEmail, // default: every sink that accepts notifications
    Priority: 1,                              // Pushover only
})
```

`rlog.Notify`, `rlog.NotifyRaw` and `rlog.Send` do the same on the Logger installed by `Init` (and do nothing before it).

Formatted messages are decorated like log notifications: the title is the program name (plus `: Title`), the email subject adds the first line of text, and emails end with the time and program. Raw messages get none of this.

## Repeated messages

With `repeats.enabled`, the first occurrence of a record is logged at once; identical records (same level, message and attributes) within `window` are counted, and when the window ends the last one is logged once with `repeated=N`. A steady flood of one message produces one line per window on every sink, including notifications. Special messages are grouped the same way (`(repeated N more times)` is added to the text). `Close` flushes pending counts.

## Failure handling

- Notifications are queued and sent on a background goroutine per notifier, so logging and `Notify` never wait on the network. When a queue fills (`queue_size`), new notifications are dropped.
- Each delivery attempt is limited to `notify_timeout`; a failure (no network, bad key, server error) is reported and the notification dropped — there are no retries. A panic inside a sender is recovered.
- rlog's own errors go to `Config.ErrorWriter` (stderr by default), never back into slog, at most one line per notifier per minute with a count of the errors suppressed in between. Telegram errors never include the bot token.
- `Close` waits at most `close_timeout` in total (notifiers drain in parallel), then aborts what is in flight and returns an error saying how many notifications may have been dropped.
- An invalid configuration (e.g. a Telegram sink without a token) is reported by `New`/`Init`; it never contacts the services, so a bad key only shows up as delivery errors later.

## Notes

- After `Close`, records still reach the console and file, but notifications are dropped.
- lumberjack always rotates by size (`max_size_mb`, 100 MB when 0). `daily` adds a rotation at midnight. Backup files are named by when they were rotated, so the file closed at midnight carries the next day's date.
- Pushover priority 2 (emergency) is sent with `retry=60` and `expire=3600`.
- Telegram's HTML mode accepts only a few tags (`<b>`, `<i>`, `<u>`, `<s>`, `<a>`, `<code>`, `<pre>`, …). Texts longer than a service's limit are cut, which can break markup.
