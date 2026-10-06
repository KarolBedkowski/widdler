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
	b   *Backuper
	dav *webdav.Handler

	user    string
	home    string
	offline bool

	mu sync.Mutex
}

func newUserHandler(user, homedir string, backuper *Backuper, offline bool) *userHomeHandler {
	slog.Debug("new user handler", "user", user, "homedir", homedir)

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
		b:       backuper,
		user:    user,
		home:    homedir,
		offline: offline,
		dav:     dav,
	}
}

func (u *userHomeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()

	resolvedPath := filepath.Join(".", r.URL.Path)
	ctx := slogctx.Append(r.Context(), slog.String("path", resolvedPath))
	slog.DebugContext(ctx, "server: resolved file")

	r = r.WithContext(ctx)

	root, err := os.OpenRoot(u.home)
	if err != nil {
		slog.ErrorContext(ctx, "server: open root error", "err", err)
		renderError(w, http.StatusInternalServerError, err)

		return
	}
	defer root.Close()

	fileinfo, err := root.Stat(resolvedPath)
	switch {
	// handle not existing path
	case os.IsNotExist(err):
		err = u.handleNotExisting(ctx, w, r, root, resolvedPath)
	// handle other stat errors
	case err != nil || fileinfo == nil:
		err = errors.WithMessagef(err, "stat %q", resolvedPath)
	// handle existing dir -> browse
	case fileinfo.IsDir():
		err = u.handleBrowse(w, r, root, resolvedPath)
	// handle existing files
	default:
		err = u.handleExistingFile(ctx, w, r, root, resolvedPath)
	}

	if err != nil {
		slog.ErrorContext(ctx, "server: handler error", "err", err)
		renderError(w, http.StatusInternalServerError, err)
	}
}

func (u *userHomeHandler) handleNotExisting(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	root *os.Root,
	resolvedPath string,
) error {
	switch {
	case u.offline:
		renderError(w, http.StatusNotFound, nil)

	// ignore non html files
	case !htmlRequested(r):
		renderError(w, http.StatusNotFound, nil)

	// accept only get method
	case r.Method != http.MethodGet:
		renderError(w, http.StatusNotFound, nil)

	default:
		// create empty wiki
		if err := createEmptyWiki(ctx, root, resolvedPath); err != nil {
			return WithPublic(errors.WithMessage(err, "create empty wiki"), "Internal error: failed create empty wiki")
		}

		// wiki created, serve
		u.dav.ServeHTTP(w, r)
	}

	return nil
}

func (u *userHomeHandler) handleExistingFile(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	root *os.Root,
	resolvedPath string,
) error {
	// require text/html content type for put/post methods
	if !validateContentType(r, "text/html") {
		http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)

		return nil
	}

	// create backup on put
	if r.Method == http.MethodPut && htmlRequested(r) {
		if err := u.b.create(ctx, root, u.user, resolvedPath); err != nil {
			return WithPublic(errors.WithMessage(err, "create backup"), "Internal error: failed create backup")
		}
	}

	u.dav.ServeHTTP(w, r)

	return nil
}

func htmlRequested(r *http.Request) bool {
	ext := filepath.Ext(r.URL.Path)

	return ext == ".html" || ext == ".htm"
}

// -------------------------------------------------------------------

func createEmptyWiki(ctx context.Context, root *os.Root, path string) error {
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

	out, err := root.OpenFile(path, os.O_RDWR|os.O_CREATE, defaultFilePerm)
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

	h := w.Header()
	h.Add("X-Frame-Options", "DENY")
	h.Add("X-Content-Type-Options", "nosniff")
	h.Add("Strict-Transport-Security", "max-age=31536000; preload")
	h.Add("X-DNS-Prefetch-Control", "off")
	h.Add("Cross-Origin-Embedder-Policy", "require-corp")
	h.Add("Cross-Origin-Resource-Policy", "same-site")
	h.Add("Permissions-Policy", "interest-cohort=()")
	h.Add("Content-Security-Policy",
		"frame-ancestors 'self'; img-src 'self'; object-src 'self'; base-uri 'self';")

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
