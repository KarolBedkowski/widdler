package internal

import (
	"context"
	"crypto/tls"
	"embed"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"gitlab.com/tozd/go/errors"
)

const emptyURL = "https://tiddlywiki.com/empty.html"

const (
	ServerReadTimeout   = 60 * time.Second
	ServerHeaderTimeout = 10 * time.Second
)

//go:embed *.ico
var staticFS embed.FS

// -------------------------------------------------------------------

type MultiUserHandler struct {
	handlers map[string]*userHomeHandler
}

func (m *MultiUserHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := UserFromCtx(r.Context())

	if h, ok := m.handlers[user]; ok {
		h.ServeHTTP(w, r)
	} else {
		w.Header().Set("WWW-Authenticate", `Basic realm="widdlerex"`)
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
	}
}

func newMultiUserHandler(basedir string, users map[string]string, backuper *Backuper, offline bool) *MultiUserHandler {
	handlers := make(map[string]*userHomeHandler, len(users))

	for user := range users {
		handlers[user] = newUserHandler(user, path.Join(basedir, user), backuper, offline)
	}

	return &MultiUserHandler{handlers: handlers}
}

// -------------------------------------------------------------------

type Server struct {
	cfg      *Config
	srv      *http.Server
	listener net.Listener
}

func newServer(cfg *Config, users map[string]string, backuper *Backuper) *Server {
	server := &Server{cfg: cfg}

	var handler http.Handler

	if cfg.Auth == AuthNone {
		handler = newUserHandler("", cfg.WikisDir, backuper, cfg.Offline)
	} else {
		handler = newMultiUserHandler(cfg.WikisDir, users, backuper, cfg.Offline)
	}

	mux := http.NewServeMux()
	mux.Handle("/", handler)
	mux.Handle("/favicon.ico", http.FileServerFS(staticFS))
	mux.Handle("/_backups/", backuper.ServerRoutes())

	var roothandler http.Handler = mux
	if cfg.Auth != AuthNone {
		roothandler = &Authenticator{mux, users}
	}

	roothandler = http.NewCrossOriginProtection().Handler(roothandler)
	roothandler = &Logger{roothandler}
	roothandler = &secMiddleware{roothandler}

	server.srv = &http.Server{ //nolint:exhaustruct
		Handler:           roothandler,
		ReadHeaderTimeout: ServerHeaderTimeout,
		ReadTimeout:       ServerReadTimeout,
	}

	return server
}

func (c *Server) StartListen(ctx context.Context) error {
	lconfig := &net.ListenConfig{} //nolint:exhaustruct

	lis, err := lconfig.Listen(ctx, "tcp", c.cfg.HTTPListen)
	if err != nil {
		return errors.WithMessage(err, "listen")
	}

	c.listener = lis

	return nil
}

func (c *Server) Run(ctx context.Context) error {
	if c.listener == nil {
		panic("no listener")
	}

	if c.cfg.TLSCert == "" || c.cfg.TLSKey == "" {
		return c.startHTTP(ctx)
	}

	return c.startHTTPS(ctx)
}

func (c *Server) Stop(error) {
	if c.listener != nil {
		c.listener.Close()
	}
}

func (c *Server) startHTTP(ctx context.Context) error {
	slog.InfoContext(ctx, "server: listening on '"+c.cfg.HTTPListen+"' (http)")

	if err := c.srv.Serve(c.listener); err != nil {
		return errors.WithMessage(err, "serve http")
	}

	slog.InfoContext(ctx, "server stopping")

	return nil
}

func (c *Server) startHTTPS(ctx context.Context) error {
	slog.InfoContext(ctx, "server: listening on '"+c.cfg.HTTPListen+"' (https)")

	c.srv.TLSConfig = &tls.Config{ //nolint:exhaustruct
		MinVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
		CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP521, tls.CurveP384, tls.CurveP256},
	}

	if err := c.srv.ServeTLS(c.listener, c.cfg.TLSCert, c.cfg.TLSKey); err != nil {
		return errors.WithMessage(err, "serve https")
	}

	return nil
}

// -------------------------------------------------------------------

type secMiddleware struct {
	next http.Handler
}

func (s *secMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Block .htpasswd, Prevent directory traversal
	if strings.Contains(r.URL.Path, ".htpasswd") {
		renderError(w, http.StatusNotFound, nil)

		return
	}

	// do not accept path contains hidden files/dirs
	if strings.Contains(r.URL.Path, "/.") {
		renderError(w, http.StatusNotFound, nil)

		return
	}

	headers := w.Header()
	headers.Add("X-Frame-Options", "DENY")
	headers.Add("X-Content-Type-Options", "nosniff")
	headers.Add("Strict-Transport-Security", "max-age=31536000; preload")
	headers.Add("X-DNS-Prefetch-Control", "off")
	headers.Add("Cross-Origin-Embedder-Policy", "require-corp")
	headers.Add("Cross-Origin-Resource-Policy", "same-site")
	headers.Add("Permissions-Policy", "interest-cohort=()")
	headers.Add("Content-Security-Policy",
		"frame-ancestors 'self'; "+
			"default-src 'self';"+
			"img-src 'self'; "+
			"object-src 'self'; "+
			"base-uri 'self'; "+
			"form-action 'self'; "+
			"script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+
			"style-src 'self' 'unsafe-inline'; ")
	headers.Add("Referrer-Policy", "same-origin")
	headers.Add("Cross-Origin-Opener-Policy", "same-origin-allow-popups")
	headers.Add("Cross-Origin-Resource-Policy", "same-site")

	s.next.ServeHTTP(w, r)
}

// -------------------------------------------------------------------

func renderError(w http.ResponseWriter, status int, err error) {
	var msg string
	if err != nil {
		msg = Public(err)
	}

	if msg == "" {
		msg = http.StatusText(status)
	}

	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	fmt.Fprintln(w, msg) //nolint:gosec,nolintlint

	if !devmode || err == nil {
		return
	}

	fmt.Fprint(w, "\n\n----------\n\n")

	fmt.Fprintln(w, err.Error()) //nolint:gosec,nolintlint

	for k, v := range errors.AllDetails(err) {
		fmt.Fprintf(w, "    %s: %v\n", k, v)
	}

	if c := errors.Cause(err); c != nil {
		fmt.Fprintf(w, "cause: %#+v\n", c)
	}

	if e, ok := errors.AsType[errors.E](err); ok {
		fmt.Fprintf(w, "\nStack:\n% +-v", e)
	}
}
