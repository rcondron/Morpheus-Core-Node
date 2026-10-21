// Package logx configures structured logging (log/slog) with a compact,
// colorized console format for humans and JSON for machines.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Options controls logger construction.
type Options struct {
	Level  string // debug|info|warn|error
	Format string // auto|pretty|json
	Writer io.Writer
}

// New builds a slog.Logger according to opts.
func New(opts Options) *slog.Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	level := ParseLevel(opts.Level)
	format := opts.Format
	if format == "" || format == "auto" {
		format = "json"
		if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			format = "pretty"
		}
	}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
	}
	color := false
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) && os.Getenv("NO_COLOR") == "" {
		color = true
	}
	return slog.New(&prettyHandler{w: w, level: level, color: color, mu: &sync.Mutex{}})
}

// ParseLevel maps a level name to a slog.Level (default info).
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Discard returns a logger that drops everything.
func Discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type prettyHandler struct {
	w      io.Writer
	level  slog.Level
	color  bool
	attrs  []slog.Attr
	groups []string
	mu     *sync.Mutex
}

func (h *prettyHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *prettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &c
}

func (h *prettyHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.groups = append(append([]string(nil), h.groups...), name)
	return &c
}

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
	ansiGreen  = "\x1b[32m"
)

func (h *prettyHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	ts := r.Time.Format(time.TimeOnly)
	lvl, col := "INF", ansiGreen
	switch {
	case r.Level >= slog.LevelError:
		lvl, col = "ERR", ansiRed
	case r.Level >= slog.LevelWarn:
		lvl, col = "WRN", ansiYellow
	case r.Level < slog.LevelInfo:
		lvl, col = "DBG", ansiCyan
	}
	if h.color {
		fmt.Fprintf(&b, "%s%s%s %s%s%s %s", ansiDim, ts, ansiReset, col, lvl, ansiReset, r.Message)
	} else {
		fmt.Fprintf(&b, "%s %s %s", ts, lvl, r.Message)
	}
	prefix := strings.Join(h.groups, ".")
	write := func(a slog.Attr) {
		if a.Equal(slog.Attr{}) {
			return
		}
		key := a.Key
		if prefix != "" {
			key = prefix + "." + key
		}
		val := a.Value.Resolve().String()
		if strings.ContainsAny(val, " \t\"") {
			val = fmt.Sprintf("%q", val)
		}
		if h.color {
			fmt.Fprintf(&b, " %s%s=%s%s", ansiDim, key, ansiReset, val)
		} else {
			fmt.Fprintf(&b, " %s=%s", key, val)
		}
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(func(a slog.Attr) bool { write(a); return true })
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}
