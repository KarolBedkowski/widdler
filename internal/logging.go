package internal

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/rs/xid"
	slogctx "github.com/veqryn/slog-context"
)

func setupLogging(level, format string) {
	lev := parseLevel(level)
	opts := &slog.HandlerOptions{Level: lev} //nolint:exhaustruct

	logFormat := "logfmt"
	isatty := isatty.IsTerminal(os.Stderr.Fd())

	if format != "" {
		logFormat = format
	} else if isatty {
		// default for console
		logFormat = "tint"
	}

	var handler slog.Handler

	switch logFormat {
	case "tint":
		handler = tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{ //nolint:exhaustruct
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

func parseLevel(s string) slog.Level {
	if s == "" {
		return slog.LevelInfo
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		slog.Error("parse log level error", "level", s, "err", err)
	}

	return level
}

// -------------------------------------------------------------------

type logResponseWriter struct {
	http.ResponseWriter // compose original http.ResponseWriter

	status int // http status
	size   int // response size
}

func (r *logResponseWriter) Write(b []byte) (int, error) {
	size, err := r.ResponseWriter.Write(b) // write response using original http.ResponseWriter
	r.size += size                         // capture size

	if err != nil {
		return size, fmt.Errorf("write response error: %w", err)
	}

	return size, nil
}

func (r *logResponseWriter) WriteHeader(status int) {
	r.ResponseWriter.WriteHeader(status)

	r.status = status
}

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
		"request_size", r.ContentLength,
	)

	lrw := logResponseWriter{ResponseWriter: w, status: 0, size: 0}
	startTS := time.Now()

	defer func() {
		if err := recover(); err != nil {
			rlog.ErrorContext(ctx,
				"request error - recovered",
				"err", err,
				"dur", time.Since(startTS),
				"stack", string(debug.Stack()))

			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		} else {
			level := slog.LevelInfo
			if lrw.status >= 500 { //nolint:mnd
				level = slog.LevelWarn
			}

			rlog.Log(ctx, level, "request finished", "dur", time.Since(startTS), "status", lrw.status,
				"response_size", lrw.size)
		}
	}()

	l.next.ServeHTTP(&lrw, r)
}
