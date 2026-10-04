package internal

import (
	"context"
	"crypto/tls"
	"embed"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	slogctx "github.com/veqryn/slog-context"
	"gitlab.com/tozd/go/errors"
	"golang.org/x/net/webdav"
)

const emptyURL = "https://tiddlywiki.com/empty.html"

const (
	ServerReadTimeout   = 60 * time.Second
	ServerHeaderTimeout = 10 * time.Second
)

//go:embed *.ico
var staticFS embed.FS

// -------------------------------------------------------------------

type userHomeHandler struct {
	b    *Backuper
	dav  *webdav.Handler
	root *os.Root

	user string
	home string
	mu   sync.Mutex
}

func newUserHandler(user, homedir string, backuper *Backuper) (*userHomeHandler, error) {
	slog.Debug("new user handler", "user", user, "homedir", homedir)

	root, err := os.OpenRoot(homedir)
	if err != nil {
		return nil, errors.WithDetails(errors.WithMessage(err, "open user home dir"),
			"user", user, "homedir", homedir)
	}

	dav := &webdav.Handler{ //nolint:exhaustruct
		LockSystem: webdav.NewMemLS(),
		FileSystem: webdav.Dir(homedir),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				slog.ErrorContext(r.Context(), "server: handle error", "req", r.URL, "err", err)
			}
		},
	}

	return &userHomeHandler{ //nolint:exhaustruct
		b:    backuper,
		user: user,
		home: homedir,
		root: root,
		dav:  dav,
	}, nil
}

func (u *userHomeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()

	ctx := slogctx.Append(r.Context(), slog.String("user", u.user))
	r = r.WithContext(ctx)

	if strings.HasSuffix(r.URL.Path, "/favicon.ico") {
		http.ServeFileFS(w, r, staticFS, "favicon.ico")

		return
	}

	if u.b.handleBackupsPage(ctx, w, r, u.root, u.user) {
		return
	}

	resolvedPath := filepath.Clean(filepath.Join(".", r.URL.Path))

	ctx = slogctx.Append(ctx, slog.String("file_path", resolvedPath))
	slog.DebugContext(ctx, "server: resolved file")

	// HTML files will be created or sent back
	switch err := u.handleHTML(w, r, resolvedPath); {
	case err == nil:
		return
	case !errors.Is(err, ErrNotFound):
		slog.ErrorContext(ctx, "server: handle html error", "path", resolvedPath, "err", err)
		renderError(w, http.StatusInternalServerError, err)

		return
	}

	// Everything else is browsable
	switch err := u.handleBrowse(w, r, resolvedPath); {
	case errors.Is(err, ErrNotFound):
		slog.InfoContext(
			ctx,
			"server: handle browse error - not found",
			"path",
			resolvedPath,
			"err",
			SlogStrippedErrValue(err),
		)
		renderError(w, http.StatusNotFound, nil)
	case err != nil:
		slog.ErrorContext(ctx, "server: handle browse error", "path", resolvedPath, "err", err)
		renderError(w, http.StatusInternalServerError, err)
	}
}

func (u *userHomeHandler) handleHTML(w http.ResponseWriter, r *http.Request, path string) error {
	if ext := filepath.Ext(r.URL.Path); ext != ".html" && ext != ".htm" {
		return ErrNotFound
	}

	ctx := r.Context()

	switch _, err := u.root.Stat(path); {
	case os.IsNotExist(err):
		// file not exists, try create empty
		slog.InfoContext(ctx, "server: creating empty wiki", "root", u.root)

		if err := createEmptyWiki(ctx, u.root, path); err != nil {
			return WithPublic(errors.WithMessage(err, "create empty wiki"),
				"Internal error: failed create empty wiki")
		}
	case err != nil:
		return errors.WithMessagef(err, "check file %q", path)
	case !validateContentType(r, "text/html"):
		// require text/html content type
		http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)

		return nil
	case r.Method == http.MethodPut:
		// no error, file exists, make backup on put
		if err := u.b.create(ctx, u.root, u.user, path); err != nil {
			return WithPublic(errors.WithMessage(err, "create backup"),
				"Internal error: failed create backup")
		}
	}

	u.dav.ServeHTTP(w, r)

	return nil
}

// -------------------------------------------------------------------

func createEmptyWiki(ctx context.Context, root *os.Root, path string) error {
	const filePerm = 0o600

	slog.InfoContext(ctx, "server: downloading", "url", emptyURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, emptyURL, http.NoBody)
	if err != nil {
		return errors.WithMessagef(err, "download %s", emptyURL)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp == nil {
		return errors.WithMessagef(err, "download %s", emptyURL)
	}

	defer resp.Body.Close()

	out, err := root.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return errors.WithMessage(err, "open output file")
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return errors.WithMessage(err, "write")
	}

	slog.InfoContext(ctx, "server: create empty wiki completed")

	return nil
}

// -------------------------------------------------------------------

type userHandlers map[string]*userHomeHandler

type MultiUserHandler struct {
	handlers userHandlers
}

func (m *MultiUserHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Block .htpasswd, Prevent directory traversal
	if strings.Contains(r.URL.Path, ".htpasswd") || strings.Contains(r.URL.Path, "..") {
		http.NotFound(w, r)

		return
	}

	user := UserFromCtx(r.Context())

	if h, ok := m.handlers[user]; ok {
		h.ServeHTTP(w, r)

		return
	}

	w.Header().Set("WWW-Authenticate", `Basic realm="widdlerex"`)
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

func newMultiUserHandler(basedir string, users map[string]string, backuper *Backuper) (*MultiUserHandler, error) {
	handlers := make(map[string]*userHomeHandler, len(users))

	for user := range users {
		// TODO : move
		homedir := filepath.Clean(path.Join(basedir, user))
		if err := ensureDirExists(homedir); err != nil {
			return nil, errors.WithMessagef(err, "ensure home %q for user %q", user, homedir)
		}

		h, err := newUserHandler(user, homedir, backuper)
		if err != nil {
			return nil, errors.WithMessagef(err, "prepare handler for user %q", user)
		}

		handlers[user] = h
	}

	if len(handlers) == 0 {
		return nil, errors.New("no user defined in password file")
	}

	return &MultiUserHandler{handlers: handlers}, nil
}

// -------------------------------------------------------------------

type Server struct {
	cfg      *Config
	srv      *http.Server
	listener net.Listener
}

func newServer(cfg *Config, users map[string]string, backuper *Backuper) (*Server, error) {
	server := &Server{cfg: cfg}

	var (
		handler http.Handler
		err     error
	)

	if cfg.Auth == AuthNone {
		handler, err = newUserHandler("", cfg.WikisDir, backuper)
	} else {
		handler, err = newMultiUserHandler(cfg.WikisDir, users, backuper)
	}

	if err != nil {
		return nil, errors.WithMessage(err, "prepare handler")
	}

	if cfg.Auth != AuthNone {
		handler = &Authenticator{handler, users}
	}

	mux := http.NewServeMux()
	mux.Handle("/favicon.ico", http.FileServerFS(staticFS))
	mux.Handle("/", &Logger{handler})

	server.srv = &http.Server{ //nolint:exhaustruct
		Handler:           mux,
		ReadHeaderTimeout: ServerHeaderTimeout,
		ReadTimeout:       ServerReadTimeout,
	}

	return server, nil
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

	fmt.Fprintln(w, msg)

	if !devmode || err == nil {
		return
	}

	fmt.Fprint(w, "\n\n----------\n\n")

	fmt.Fprintln(w, err.Error())

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
