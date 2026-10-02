package internal

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli/v3"
	slogctx "github.com/veqryn/slog-context"
	"gitlab.com/tozd/go/errors"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/webdav"
)

const emptyURL = "https://tiddlywiki.com/empty.html"

const (
	AuthBasic = "basic"
	AuthNone  = "none"
)

const (
	ServerReadTimeout   = 60 * time.Second
	ServerHeaderTimeout = 10 * time.Second
)

//go:embed *.ico
var staticFS embed.FS

// -------------------------------------------------------------------

type userHomeHandler struct {
	fs   http.Handler
	b    *Backuper
	dav  *webdav.Handler
	root *os.Root

	user string
	// pass is hashed user password.
	pass string
	home string
	mu   sync.Mutex
}

func newUserHandler(user, pass, homedir string, backuper *Backuper) (*userHomeHandler, error) {
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
		pass: pass,
		home: homedir,
		root: root,
		dav:  dav,
		fs:   http.FileServerFS(root.FS()),
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

func (u *userHomeHandler) authenticate(pass string) bool {
	return bcrypt.CompareHashAndPassword([]byte(u.pass), []byte(pass)) == nil
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

		if err := createEmpty(ctx, u.root, path); err != nil {
			return WithPublic(errors.WithMessage(err, "create empty wiki"),
				"Internal error: failed create empty wiki")
		}
	case err != nil:
		return errors.WithMessagef(err, "check file %q", path)
	case r.Method == http.MethodPut:
		// no error, file exists, make backup on put
		if err := u.b.create(ctx, u.root, u.user, path); err != nil {
			return WithPublic(errors.WithMessage(err, "create backup"),
				"Internal error: failed create backup")
		}
	}

	// require text/html content type
	if !validateContentType(r, "text/html") {
		http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)
	} else {
		u.dav.ServeHTTP(w, r)
	}

	return nil
}

// -------------------------------------------------------------------

func createEmpty(ctx context.Context, root *os.Root, path string) error {
	const filePerm = 0o600

	slog.InfoContext(ctx, "server: downloading", "url", emptyURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, emptyURL, http.NoBody)
	if err != nil {
		return errors.WithMessage(err, "download %s", emptyURL)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp == nil {
		return errors.WithMessage(err, "download %s", emptyURL)
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
	auth     string
}

func (m *MultiUserHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Block .htpasswd, Prevent directory traversal
	if strings.Contains(r.URL.Path, ".htpasswd") || strings.Contains(r.URL.Path, "..") {
		http.NotFound(w, r)

		return
	}

	user, pass, _ := r.BasicAuth()
	switch h, ok := m.handlers[user]; {
	case user == "":
		// do no log initial, pre-auth errors
	case !ok:
		slog.InfoContext(r.Context(), "server: auth failed", "user", user, "reason", "unknown user")
	case ok && h.authenticate(pass):
		h.ServeHTTP(w, r)

		return
	default:
		slog.InfoContext(r.Context(), "server: auth failed", "user", user, "reason", "wrong password")
	}

	w.Header().Set("WWW-Authenticate", `Basic realm="widdlerex"`)
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

func (m *MultiUserHandler) loadUsers(davDir, htpassPath string, backuper *Backuper) error {
	if _, fErr := os.Stat(htpassPath); os.IsNotExist(fErr) {
		return errors.New("no password file found")
	}

	p, err := os.Open(htpassPath)
	if err != nil {
		return errors.WithMessagef(err, "open users file %q", htpassPath)
	}

	defer p.Close()

	ht := csv.NewReader(p)
	ht.Comma = ':'
	ht.Comment = '#'
	ht.TrimLeadingSpace = true

	entries, err := ht.ReadAll()
	if err != nil {
		return errors.WithMessagef(err, "read users from %q", htpassPath)
	}

	m.handlers = make(map[string]*userHomeHandler, len(entries))

	for _, parts := range entries {
		if parts[0] == "" || parts[1] == "" { // skip entries with empty user and/or password
			continue
		}

		homedir := filepath.Clean(path.Join(davDir, parts[0]))
		if err := ensureHomeExists(homedir); err != nil {
			return errors.WithMessage(err, "ensure home %q for user %q", parts[0], homedir)
		}

		h, err := newUserHandler(parts[0], parts[1], homedir, backuper)
		if err != nil {
			return errors.WithMessage(err, "prepare handler for user %q", parts[0])
		}

		m.handlers[parts[0]] = h
	}

	return nil
}

// -------------------------------------------------------------------

func ensureHomeExists(home string) error {
	const homeDirPerm = 0o700

	switch s, err := os.Stat(home); {
	case err == nil:
	case s == nil || os.IsNotExist(err):
		slog.Info("creating home dir", "home", home)

		if err := os.Mkdir(home, homeDirPerm); err != nil {
			return errors.WithMessage(err, "make home ")
		}
	default:
		return errors.WithMessage(err, "check home")
	}

	return nil
}

// -------------------------------------------------------------------

type Server struct {
	authMethod string
	davDir     string
	listen     string
	htpassPath string
	tlsCert    string
	tlsKey     string
}

func newServer(cmd *cli.Command) Server {
	auth := cmd.String("auth")
	if auth == "none" {
		auth = AuthNone
	}

	server := Server{
		davDir:     cmd.String("wikis"),
		listen:     cmd.String("http"),
		tlsCert:    cmd.String("tlscert"),
		tlsKey:     cmd.String("tlskey"),
		htpassPath: filepath.Clean(cmd.String("htpass")),
		authMethod: auth,
	}

	return server
}

func (c *Server) Validate() error {
	var err error

	c.davDir, err = filepath.Abs(c.davDir)
	if err != nil {
		return errors.WithMessagef(err, "invalid wikis dir %q", c.davDir)
	}

	if c.authMethod != AuthNone && c.authMethod != AuthBasic {
		return errors.New("invalid auth type")
	}

	return nil
}

func (c *Server) Start(ctx context.Context, backuper *Backuper) error {
	handler, users, err := c.setupHandlers(backuper)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/favicon.ico", http.FileServerFS(staticFS))
	mux.Handle("/", &Logger{handler})

	srv := http.Server{ //nolint:exhaustruct
		Handler:           mux,
		ReadHeaderTimeout: ServerHeaderTimeout,
		ReadTimeout:       ServerReadTimeout,
	}

	lconfig := &net.ListenConfig{} //nolint:exhaustruct

	lis, err := lconfig.Listen(ctx, "tcp", c.listen)
	if err != nil {
		return errors.WithMessage(err, "start listen")
	}

	backuper.start(ctx, users)

	if c.tlsCert == "" || c.tlsKey == "" {
		return c.startHTTP(ctx, &srv, lis)
	}

	return c.startHTTPS(ctx, &srv, lis)
}

func (c *Server) startHTTP(ctx context.Context, srv *http.Server, lis net.Listener) error {
	slog.InfoContext(ctx, "server: listening on '"+c.listen+"' (http)")

	if err := srv.Serve(lis); err != nil {
		return errors.WithMessage(err, "serve http")
	}

	return nil
}

func (c *Server) startHTTPS(ctx context.Context, srv *http.Server, lis net.Listener) error {
	slog.InfoContext(ctx, "server: listening on '"+c.listen+"' (https)")

	srv.TLSConfig = &tls.Config{ //nolint:exhaustruct
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

	if err := srv.ServeTLS(lis, c.tlsCert, c.tlsKey); err != nil {
		return errors.WithMessage(err, "serve https")
	}

	return nil
}

func (c *Server) setupHandlers(backuper *Backuper) (http.Handler, []string, error) {
	if c.authMethod == AuthNone {
		h, err := newUserHandler("", "", c.davDir, backuper)
		if err != nil {
			return nil, nil, errors.WithMessage(err, "prepare handler")
		}

		return h, nil, nil
	}

	m := &MultiUserHandler{auth: c.authMethod} //nolint:exhaustruct
	if err := m.loadUsers(c.davDir, c.htpassPath, backuper); err != nil {
		return nil, nil, errors.WithMessage(err, "load users")
	}

	return m, slices.Collect(maps.Keys(m.handlers)), nil
}

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
