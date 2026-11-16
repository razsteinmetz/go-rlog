package rlog

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// repeatState tracks recently seen records (or messages) by key and decides
// which to pass through. See RepeatConfig for the behavior.
type repeatState struct {
	window time.Duration

	mu      sync.Mutex
	closed  bool                    // after close everything passes through
	entries map[string]*repeatEntry // open grouping windows by key
}

// repeatEntry is one open grouping window.
type repeatEntry struct {
	count int             // repeats suppressed in the current window
	flush func(count int) // emits the summary for the latest repeat
	timer *time.Timer     // fires at the end of the window
}

// newRepeatState returns an empty state with the given window.
func newRepeatState(window time.Duration) *repeatState {
	return &repeatState{window: window, entries: make(map[string]*repeatEntry)}
}

// allow reports whether an occurrence of key should be emitted now. When it
// returns false the occurrence was counted, and flush (which replaces any
// earlier one for key) will be called with the count when the window ends.
func (s *repeatState) allow(key string, flush func(count int)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return true
	}
	if e, ok := s.entries[key]; ok {
		e.count++
		e.flush = flush
		return false
	}
	e := &repeatEntry{}
	s.entries[key] = e
	e.timer = time.AfterFunc(s.window, func() { s.expire(key, e) })
	return true
}

// expire ends e's window. If repeats were counted, it emits the summary and
// opens another window for key; otherwise it forgets key.
func (s *repeatState) expire(key string, e *repeatEntry) {
	s.mu.Lock()
	if s.closed || s.entries[key] != e {
		s.mu.Unlock()
		return
	}
	count, flush := e.count, e.flush
	if count == 0 {
		delete(s.entries, key)
		s.mu.Unlock()
		return
	}
	e.count, e.flush = 0, nil
	e.timer.Reset(s.window)
	s.mu.Unlock()
	flush(count) // outside the lock: it calls into the sinks
}

// close stops all timers, emits every pending summary and makes allow pass
// everything from now on.
func (s *repeatState) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	type pending struct {
		flush func(int)
		count int
	}
	var todo []pending
	for _, e := range s.entries {
		e.timer.Stop()
		if e.count > 0 {
			todo = append(todo, pending{e.flush, e.count})
		}
	}
	s.entries = nil
	s.mu.Unlock()
	for _, p := range todo {
		p.flush(p.count)
	}
}

// repeatHandler wraps the fan-out handler and drops records that repeat
// within the window, emitting a summary record instead.
type repeatHandler struct {
	next   slog.Handler // the wrapped handler
	st     *repeatState // shared by all handlers derived from one Logger
	prefix []byte       // attrs added with WithAttrs, formatted, part of the key
	group  string       // open groups as a "g1.g2." key prefix
}

// Enabled defers to the wrapped handler.
func (h *repeatHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

// WithAttrs returns a handler whose key also covers attrs.
func (h *repeatHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := *h
	h2.next = h.next.WithAttrs(attrs)
	h2.prefix = append([]byte(nil), h.prefix...)
	for _, a := range attrs {
		h2.prefix = appendAttr(h2.prefix, h.group, a, false)
	}
	return &h2
}

// WithGroup returns a handler with the group open.
func (h *repeatHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.next = h.next.WithGroup(name)
	h2.group = h.group + name + "."
	return &h2
}

// Handle passes r on unless it repeats a record seen within the window.
// The key is the level, message and all attributes; time and source are
// ignored.
func (h *repeatHandler) Handle(ctx context.Context, r slog.Record) error {
	key := make([]byte, 0, 64+len(r.Message)+len(h.prefix))
	key = append(key, r.Level.String()...)
	key = append(key, 0)
	key = append(key, r.Message...)
	key = append(key, 0)
	key = append(key, h.prefix...)
	r.Attrs(func(a slog.Attr) bool {
		key = appendAttr(key, h.group, a, false)
		return true
	})

	saved := r.Clone()
	next := h.next
	pass := h.st.allow(string(key), func(count int) {
		s := saved.Clone()
		s.AddAttrs(slog.Int(RepeatedKey, count))
		_ = next.Handle(context.Background(), s)
	})
	if !pass {
		return nil
	}
	return h.next.Handle(ctx, r)
}
