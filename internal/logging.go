package internal

import (
	"context"
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
	"gitlab.com/tozd/go/errors"
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
		handler = tint.NewTextHandler(colorable.NewColorable(os.Stderr), &tint.Options{ //nolint:exhaustruct
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

	handler = slogctx.NewHandler(NewFormatterHandler(handler), nil)

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
		return size, errors.Wrap(err, "write response")
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
		"content_type", r.Header.Get("Content-Type"),
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

// ----------------------------------------------------------------------------

func SlogErrAttr(err error) slog.Attr {
	return slog.Any("err", SlogErrValue(err))
}

func SlogStrippedErrAttr(err error) slog.Attr {
	return slog.Any("err", SlogStrippedErrValue(err))
}

func SlogStrippedErrValue(err error) slog.Value {
	if err == nil {
		return slog.Value{}
	}

	attrs := []slog.Attr{
		slog.String("error", err.Error()),
	}

	if d := errors.AllDetails(err); len(d) > 0 {
		dattrs := make([]slog.Attr, 0, len(d))
		for k, v := range d {
			dattrs = append(dattrs, slog.Any(k, v))
		}

		attrs = append(attrs, slog.GroupAttrs("details", dattrs...))
	}

	if c := errors.Cause(err); c != nil {
		attrs = append(attrs, slog.Any("cause", c))
	}

	if e, ok := errors.AsType[errors.E](err); ok {
		attrs = append(attrs, slog.String("stack", fmt.Sprintf("%v", e)))
	}

	return slog.GroupValue(attrs...)
}

func SlogErrValue(err error) slog.Value {
	attrs := []slog.Attr{
		slog.String("error", err.Error()),
	}

	if d := errors.AllDetails(err); len(d) > 0 {
		dattrs := make([]slog.Attr, 0, len(d))
		for k, v := range d {
			dattrs = append(dattrs, slog.Any(k, v))
		}

		attrs = append(attrs, slog.GroupAttrs("details", dattrs...))
	}

	if c := errors.Cause(err); c != nil {
		attrs = append(attrs, slog.Any("cause", c))
	}

	if e, ok := errors.AsType[errors.E](err); ok {
		attrs = append(attrs, slog.String("stack", fmt.Sprintf("% +-v", e)))
	}

	return slog.GroupValue(attrs...)
}

// ----------------------------------------------------------------------------

// ErrorFormatterHandler format fields with error type.
type ErrorFormatterHandler struct {
	handler slog.Handler
}

// NewFormatterHandler returns a slog.Handler that applies formatters to.
func NewFormatterHandler(handler slog.Handler) slog.Handler {
	return &ErrorFormatterHandler{
		handler: handler,
	}
}

// Enabled implements slog.Handler.
func (h *ErrorFormatterHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.handler.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *ErrorFormatterHandler) Handle(ctx context.Context, r slog.Record) error {
	r2 := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(attr slog.Attr) bool {
		r2.AddAttrs(h.transform(attr))

		return true
	})

	return h.handler.Handle(ctx, r2) //nolint:wrapcheck
}

// WithAttrs implements slog.Handler.
func (h *ErrorFormatterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	for i := range attrs {
		h.transform(attrs[i])
	}

	return &ErrorFormatterHandler{
		handler: h.handler.WithAttrs(attrs),
	}
}

// WithGroup implements slog.Handler.
func (h *ErrorFormatterHandler) WithGroup(name string) slog.Handler {
	return &ErrorFormatterHandler{
		handler: h.handler.WithGroup(name),
	}
}

func (h *ErrorFormatterHandler) transform(attr slog.Attr) slog.Attr {
	if err, ok := attr.Value.Any().(error); ok {
		attr.Value = SlogErrValue(err)
	}

	return attr
}
