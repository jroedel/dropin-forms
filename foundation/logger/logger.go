// Package logger builds the one logger the whole binary shares.
//
// It knows nothing about forms, submissions or payments -- it is here so that
// two apps asking for a logger get the same one, configured in one place. The
// handler is text rather than JSON on purpose: these logs are read by a person
// tailing a file over SSH, not shipped to an aggregator, and a person reads
// key=value faster than they read braces.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Level turns a configured string into a slog level, so that a typo in
// config.toml fails at startup with a sentence naming the acceptable values
// rather than silently selecting info.
func Level(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}

	return 0, fmt.Errorf("unknown log level %q: use debug, info, warn or error", s)
}

// New returns the shared logger. Source positions are deliberately off: the
// interesting location for a request log line is the route, which the line
// already carries, and AddSource on every line is noise that makes the
// important fields harder to find.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
	}))
}
