package main

import (
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"github.com/rs/xid"
	slogctx "github.com/veqryn/slog-context"
)

func setupLogging(level, format *string) {
	lev := parseLevel(level)
	opts := &slog.HandlerOptions{Level: lev} //nolint:exhaustruct

	logFormat := "logfmt"
	isatty := isatty.IsTerminal(os.Stderr.Fd())

	if format != nil && *format != "" {
		logFormat = *format
	} else if isatty {
		// default for console
		logFormat = "tint"
	}

	var handler slog.Handler

	switch logFormat {
	case "tint":
		handler = tint.NewHandler(os.Stderr, &tint.Options{ //nolint:exhaustruct
			AddSource:  true,
			Level:      parseLevel(level),
			NoColor:    !isatty,
			TimeFormat: time.TimeOnly,
		})
	case "json":
		handler = slog.NewJSONHandler(os.Stdout, opts)
	default:
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	handler = slogctx.NewHandler(handler, nil)

	slog.SetDefault(slog.New(handler))
}

func parseLevel(s *string) slog.Level {
	if s == nil {
		return slog.LevelInfo
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*s)); err != nil {
		slog.Error("parse log level error", "level", *s, "err", err)
	}

	return level
}

// -------------------------------------------------------------------

type Logger struct {
	next http.Handler
}

func (l *Logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := xid.New().String()
	ctx := slogctx.Prepend(r.Context(), slog.String("request_id", requestID))

	r = r.WithContext(ctx)

	rlog := slog.With(
		"remote", r.RemoteAddr,
		"method", r.Method,
		"path", r.URL.Path,
		"proto", r.Proto,
		"content_length", r.ContentLength,
	)

	startTS := time.Now()

	defer func() {
		if err := recover(); err != nil {
			rlog.ErrorContext(ctx, "request error - recovered", "err", err, "dur", time.Since(startTS),
				"stack", string(debug.Stack()))
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("internal server error")) //nolint:errcheck
		} else {
			rlog.Info("request finished", "dur", time.Since(startTS))
		}
	}()

	l.next.ServeHTTP(w, r)
}
