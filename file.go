package rlog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// openFile opens the rotating log file and, for daily rotation, starts a
// goroutine that rotates at each midnight. The returned func stops that
// goroutine and closes the file.
func openFile(c FileSink, utc bool) (io.Writer, func() error, error) {
	if c.Path == "" {
		return nil, nil, errors.New("rlog: file sink enabled but path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return nil, nil, fmt.Errorf("rlog: create log dir: %w", err)
	}
	// lumberjack opens the file on the first write, and slog discards write
	// errors, so open it once here: a bad path or missing permission is
	// then reported at startup instead of silently losing the log.
	f, err := os.OpenFile(c.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("rlog: open log file: %w", err)
	}
	f.Close()
	lj := &lumberjack.Logger{
		Filename:   c.Path,
		MaxSize:    positive(c.MaxSizeMB, defaultMaxSizeMB),
		MaxBackups: c.MaxBackups,
		MaxAge:     c.MaxAgeDays,
		Compress:   c.Compress,
		LocalTime:  !utc,
	}

	stop := make(chan struct{}) // closed to stop the rotation goroutine
	done := make(chan struct{}) // closed when it has stopped
	if c.Daily {
		go func() {
			defer close(done)
			for {
				t := time.NewTimer(untilMidnight(time.Now(), utc))
				select {
				case <-t.C:
					_ = lj.Rotate()
				case <-stop:
					t.Stop()
					return
				}
			}
		}()
	} else {
		close(done)
	}

	closeFn := func() error {
		close(stop)
		<-done
		return lj.Close()
	}
	return lj, closeFn, nil
}

// untilMidnight returns the time from now to the next midnight, in UTC or
// in now's location.
func untilMidnight(now time.Time, utc bool) time.Duration {
	if utc {
		now = now.UTC()
	}
	y, m, d := now.Date()
	next := time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
	return next.Sub(now)
}
