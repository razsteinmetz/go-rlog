# go-rlog

[![Go Reference](https://pkg.go.dev/badge/github.com/razsteinmetz/go-rlog.svg)](https://pkg.go.dev/github.com/razsteinmetz/go-rlog)
[![Go version](https://img.shields.io/github/go-mod/go-version/razsteinmetz/go-rlog)](go.mod)
[![Version](https://img.shields.io/github/v/tag/razsteinmetz/go-rlog?sort=semver&label=version)](https://github.com/razsteinmetz/go-rlog/tags)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

rlog is a handler for Go's standard `log/slog` package, made for small programs such as home automation and IoT services. You log with plain `slog` calls; rlog writes each line to the console and a rotating file, and can send it to your phone or inbox.

| Sink     | What it gets |
|----------|--------------|
| Console  | Text (optionally colored) or JSON, on stderr |
| File     | Text or JSON; rotated daily and at 100 MB, 7 old files kept, gzipped |
| Telegram | A bot message to one chat |
| Pushover | A push notification, with the priority chosen by level |
| Email    | SMTP (STARTTLS, TLS or plain) to one or more recipients |

rlog carries two kinds of output:

- **Log records** are ordinary `slog.Info`, `slog.Error` and so on. Each sink has a minimum `level`: by default the console and file take `info` and above, and Telegram, Pushover and email take only errors.
- **Notifications** are messages without a level, sent with `rlog.Notify`. They are for things that are not errors but that you want to hear about: "backup finished", "garage door left open". By default they go to every sink.

Telegram, Pushover and email are called the *services* below. They are sent in the background, so a slow or unreachable service never blocks your program.

## Install

```
go get github.com/razsteinmetz/go-rlog@latest
```

Requires Go 1.25 or newer.

## Quick start

`rlog.yaml`:

```yaml
program: garage

file:
  enabled: true
  path: logs/garage.log

telegram:
  enabled: true
  token: ${TELEGRAM_TOKEN}      # taken from the environment
  chat_id: ${TELEGRAM_CHAT_ID}
```

`main.go`:

```go
package main

import (
	"log/slog"

	rlog "github.com/razsteinmetz/go-rlog"
)

func main() {
	l, err := rlog.InitFile("rlog.yaml") // read the config and make rlog slog's default
	if err != nil {
		panic(err)
	}
	defer l.Close() // send what is still queued, close the file

	slog.Info("started", "version", "1.2.0")

	door := rlog.Component("door") // a logger whose lines are marked [door]
	door.Error("sensor failed", "pin", 4)

	rlog.Notify("door left open", "minutes", 15)
}
```

Console and file:

```
2026-11-16 10:04:05.123 INFO  garage: started version=1.2.0
2026-11-16 10:04:05.124 ERROR garage: [door] sensor failed pin=4
2026-11-16 10:04:05.124 NOTE  garage: door left open minutes=15
```

Telegram receives two messages. The error, because the sink's level is `error`:

```
garage ERROR
[door] sensor failed pin=4
```

and the notification:

```
garage
door left open minutes=15
```

The [`example`](example) folder has a complete program that exercises every feature; see [Example program](#example-program).

## Log records

Log with `slog` as usual. Values are given as key/value pairs after the message:

```go
slog.Warn("temperature high", "celsius", 31.5)
slog.Error("read failed", "sensor", "attic", "err", err)
```

A text line is `time LEVEL program: [component] message key=value ... (file.go:line)`. Each part can be turned off under `format` in the config, and `source` (the `file.go:line` part) is off by default.

- **Levels.** `debug`, `info`, `warn` and `error`. A sink writes a record when the record's level is at or above the sink's `level`.
- **Components.** `rlog.Component("door")` returns a logger that marks its lines with `[door]`. It is short for `slog.Default().With("component", "door")`, so call it after `InitFile`, not in a package-level `var`.
- **The standard `log` package.** After `InitFile`, lines printed with `log.Print` (by your code or by a library) are captured too, at `info`.
- **On the services**, a record's title is `garage ERROR` and its body is `[door] sensor failed pin=4`. Pushover's priority follows the level (`priorities` in the config).

## Notifications

```go
rlog.Notify("backup finished", "files", 1200) // text "backup finished files=1200"; the title is the program name
rlog.NotifyRaw("Server up", "All good.")      // title and text exactly as given; the title may be ""
rlog.Send(rlog.Message{                       // full control
	Title:    "Daily report",
	Text:     "<b>3</b> errors today",
	HTML:     true,                           // Telegram and Pushover HTML, text/html email
	To:       rlog.ToTelegram | rlog.ToEmail, // only these sinks
	Priority: 1,                              // Pushover priority, -2..2
})
```

These use the logger installed by `InitFile` or `Init`, and do nothing before that. The same three exist as methods on the `*rlog.Logger`.

### Where a notification goes

A notification ignores the sinks' `level`.

- Without `To` (always the case for `Notify` and `NotifyRaw`) it goes to **every enabled sink**.
- With `To` it goes only to the sinks named: `rlog.ToConsole`, `ToFile`, `ToTelegram`, `ToPushover`, `ToEmail`, combined with `|`.
- A sink with `notifications: false` in the config never gets notifications, even when named in `To`.

So each sink has two independent switches:

| Sink setting | Log records | Notifications |
|--------------|-------------|---------------|
| (default) | at `level` and above | yes |
| `notifications: false` | at `level` and above | no |
| `messages_only: true` (services only) | no | yes |

### What a notification looks like

On the console and in the file it is one `NOTE` line. A title is written before the text (`Server up: All good.`), and line breaks become ` | `. In JSON output the level is `"NOTE"` and the title is a separate `title` key.

On the services it depends on whether the notification is formatted (`Notify`, or `Send` without `Raw`) or raw (`NotifyRaw`, or `Send` with `Raw: true`):

|               | Formatted | Raw |
|---------------|-----------|-----|
| Title         | the program name, or `program: Title` | `Title` exactly, or none |
| Email subject | the title; when no `Title` was given and the text is not HTML, the first line of the text is added | `Title`, or the first line of the text |
| Email body    | the text, then a line with the time and program | the text only |

## Configuration

`rlog.InitFile(path)` reads YAML, or JSON when the file name ends in `.json`.

- The file is applied on top of the defaults, so it only needs the settings you change.
- An unknown key is an error, which catches typos.
- Levels are written `debug`, `info`, `warn`, `error` (or in between, like `ERROR+4`). Durations are written `30s`, `1m`, `500ms`.
- In any text value, `${NAME}` is replaced by the environment variable `NAME`, and `${NAME:-fallback}` uses `fallback` when it is not set. This keeps tokens and passwords out of the file.

Every key, with its default (the names, paths and addresses are examples):

```yaml
program: garage          # shown in log lines and titles; default: the executable's name

format:                  # how console and file lines look
  time: full             # see "Time formats"
  utc: false             # true: UTC instead of local time
  source: ""             # "" (off) | short (main.go:42) | full (whole path)
  level: true            # show INFO, ERROR, ...
  component: true        # show [component]
  program: true          # show the program name

console:
  enabled: true
  level: info
  json: false            # true: one JSON object per line
  color: auto            # never | auto (only on a terminal) | always
  notifications: true

file:
  enabled: false
  level: info
  json: false
  notifications: true
  path: logs/garage.log  # required when enabled; missing folders are created
  max_size_mb: 100       # rotate at this size
  daily: true            # also rotate at midnight
  max_backups: 7         # rotated files to keep; 0 keeps all
  max_age_days: 0        # delete rotated files older than this; 0 never
  compress: true         # gzip rotated files

repeats:                 # see "Repeated messages"
  enabled: false
  window: 1m

queue_size: 100          # per service; when full, new items are dropped
notify_timeout: 15s      # limit on one send to a service
close_timeout: 5s        # how long Close waits for the queues to empty

telegram:
  enabled: false
  level: error
  notifications: true
  messages_only: false
  token: ${TELEGRAM_TOKEN}       # bot token from @BotFather
  chat_id: ${TELEGRAM_CHAT_ID}   # user, group or channel id

pushover:
  enabled: false
  level: error
  notifications: true
  messages_only: false
  token: ${PUSHOVER_TOKEN}       # application token
  user: ${PUSHOVER_USER}         # user or group key
  device: ""                     # optional device name(s), comma separated
  priorities: {DEBUG: -2, INFO: -1, WARN: 0, ERROR: 1}   # level: Pushover priority (-2..2)

email:
  enabled: false
  level: error
  notifications: true
  messages_only: false
  host: smtp.gmail.com
  port: 0                        # 0: 587 for starttls, 465 for tls, 25 for none
  security: starttls             # starttls | tls | none
  username: ${SMTP_USERNAME}     # empty: no login
  password: ${SMTP_PASSWORD}
  from: "Garage <bot@example.com>"
  to: [me@example.com]
```

### Time formats

`format.time` is one of these names, or any Go time layout such as `Jan _2 15:04`.

| Name          | Example |
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
| `unix`        | `1794816245` (a number in JSON output) |
| `unixms`      | `1794816245123` (a number in JSON output) |
| `none`        | no time |

### Configuring in code

The same settings are fields of `rlog.Config`:

```go
cfg := rlog.DefaultConfig()
cfg.Program = "garage"
cfg.Format.Time = rlog.TimeShort
cfg.File.Enabled = true
cfg.File.Path = "logs/garage.log"
cfg.Telegram.Enabled = true
cfg.Telegram.Token = os.Getenv("TELEGRAM_TOKEN")
cfg.Telegram.ChatID = os.Getenv("TELEGRAM_CHAT_ID")
l, err := rlog.Init(cfg)
```

`rlog.LoadConfig(path)` returns the `Config` from a file so you can adjust it before `Init`. `rlog.New(cfg)` builds a logger without making it slog's default; log through `l.Slog()`.

## Repeated messages

With `repeats.enabled: true`, a message that repeats does not fill the log or your phone:

- The first occurrence is written at once.
- Identical records (same level, message and values) within `window` are only counted.
- When the window ends, one line is written with `repeated=N`, and a new window starts.

A sensor that fails every second therefore produces one line and one alert per window. Notifications are grouped the same way; the summary's text ends with `(repeated N more times)`. `Close` writes any count still pending.

Grouping applies to all sinks at once, including the console.

## When a service fails

- **Your program is never blocked.** Each service has its own queue and background sender. When a queue is full (`queue_size`), new items are dropped.
- **No retries.** One send may take up to `notify_timeout`. If it fails (no network, wrong key, server error), the item is dropped and the error is reported.
- **Errors go to stderr**, never back into the log, as lines starting with `rlog:`. Each service prints at most one error line per minute, with a count of the ones held back.
- **A wrong key is only noticed when sending.** `InitFile` checks that the settings are complete and that the log file can be opened, but it does not contact the services.
- **`Close` waits at most `close_timeout`**, then gives up and returns an error saying how much may have been dropped.

## Good to know

- Call `Close` before the program ends. `os.Exit` and `log.Fatal` skip deferred calls, so anything still queued for the services is lost.
- After `Close`, records still reach the console and file; nothing more is sent to the services.
- Daily rotation happens only while the program is running at midnight. A program that is started and stopped during the day rotates by size only.
- A rotated file is named after the moment it was rotated, so the file closed at midnight carries the next day's date.
- Email logs in with the PLAIN method. That works with Gmail (use an app password) and most providers, but not with Outlook or Office 365.
- Every email opens its own connection, which takes a second or two. Raise `close_timeout` if the program sends several emails just before it exits.
- Texts longer than a service allows are cut: 4096 characters on Telegram, 1024 on Pushover. With `HTML: true` the cut can break the markup. Telegram accepts only a few tags (`<b>`, `<i>`, `<u>`, `<s>`, `<a>`, `<code>`, `<pre>`).
- Pushover priority 2 (emergency) repeats every 60 seconds for an hour until acknowledged.
- A `Config` holds your tokens and passwords, so do not print or log it.

## Example program

The [`example`](example) folder shows records at every level, components, repeated messages, a burst of 200 identical notifications, and each kind of notification, using the config in `example/rlog.yaml`.

```
cd example
copy .var.example .var     # cp on Linux and macOS
go run .
```

Fill `.var` with the tokens and passwords for the services you want to try; git ignores that file. A service whose values are left empty is skipped, so you can test one at a time.

## License

MIT. See [LICENSE](LICENSE).
