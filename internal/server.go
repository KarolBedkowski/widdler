package internal

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/csv"
	"errors"
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

type userHandler struct {
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

func newUserHandler(user, pass, homedir string, backuper *Backuper) *userHandler {
	slog.Debug("new user handler", "user", user, "homedir", homedir)

	root, err := os.OpenRoot(homedir)
	if err != nil {
		slog.Error("server: open home dir failed", "user", user, "homedir", homedir, "err", err)
		os.Exit(1)
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

	return &userHandler{ //nolint:exhaustruct
		b:    backuper,
		user: user,
		pass: pass,
		home: homedir,
		root: root,
		dav:  dav,
		fs:   http.FileServerFS(root.FS()),
	}
}

func (u *userHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()

	ctx := slogctx.Append(r.Context(), slog.String("user", u.user))
	r = r.WithContext(ctx)

	if u.b.handleBackupsPage(ctx, w, r, u.root, u.user) {
		return
	}

	resolvedPath := path.Join(".", filepath.FromSlash(filepath.Clean("/"+strings.Trim(r.URL.Path, "/"))))
	if resolvedPath == "" {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)

		return
	}

	ctx = slogctx.Append(ctx, slog.String("file_path", resolvedPath))
	slog.DebugContext(ctx, "server: resolved file")

	// HTML files will be created or sent back
	switch err := u.handleHTML(w, r, resolvedPath); {
	case err == nil:
		return
	case !errors.Is(err, ErrNotFound):
		slog.ErrorContext(ctx, "server: handle html error", "path", resolvedPath, "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	// Everything else is browsable
	switch err := u.handleBrowse(w, r, resolvedPath); {
	case errors.Is(err, ErrNotFound):
		slog.ErrorContext(ctx, "server: handle browse error - not found", "path", resolvedPath, "err", err)
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	case err != nil:
		slog.ErrorContext(ctx, "server: handle browse error", "path", resolvedPath, "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

func (u *userHandler) authenticate(pass string) bool {
	return bcrypt.CompareHashAndPassword([]byte(u.pass), []byte(pass)) == nil
}

var ErrNotFound = errors.New("not found")

func (u *userHandler) handleHTML(w http.ResponseWriter, r *http.Request, path string) error {
	if ext := filepath.Ext(r.URL.Path); ext != ".html" && ext != ".htm" {
		return ErrNotFound
	}

	ctx := r.Context()

	switch _, err := u.root.Stat(path); {
	case os.IsNotExist(err):
		// file not exists, try create empty
		slog.InfoContext(ctx, "server: creating empty wiki", "root", u.root)

		if err := createEmpty(ctx, u.root, path); err != nil {
			return fmt.Errorf("create empty wiki error: %w", err)
		}
	case err != nil:
		return fmt.Errorf("check file %q error: %w", path, err)
	case r.Method == http.MethodPut:
		// no error, file exists, make backup on put
		if err := u.b.create(ctx, u.root, u.user, path); err != nil {
			return fmt.Errorf("create backup error: %w", err)
		}
	}

	u.dav.ServeHTTP(w, r)

	return nil
}

// -------------------------------------------------------------------

func createEmpty(ctx context.Context, root *os.Root, path string) error {
	const filePerm = 0o600

	slog.InfoContext(ctx, "server: downloading", "url", emptyURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, emptyURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("download %s error: %w", emptyURL, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp == nil {
		return fmt.Errorf("download %s error: %w", emptyURL, err)
	}

	defer resp.Body.Close()

	out, err := root.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return fmt.Errorf("open output file error: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("write error: %w", err)
	}

	slog.InfoContext(ctx, "server: create empty wiki completed")

	return nil
}

// -------------------------------------------------------------------

type userHandlers map[string]*userHandler

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
		return errors.New("no password file found") //nolint:err113
	}

	p, err := os.Open(htpassPath)
	if err != nil {
		return fmt.Errorf("open users file %q error: %w", htpassPath, err)
	}

	defer p.Close()

	ht := csv.NewReader(p)
	ht.Comma = ':'
	ht.Comment = '#'
	ht.TrimLeadingSpace = true

	entries, err := ht.ReadAll()
	if err != nil {
		return fmt.Errorf("read users from %q error: %w", htpassPath, err)
	}

	m.handlers = make(map[string]*userHandler, len(entries))

	for _, parts := range entries {
		if parts[0] == "" || parts[1] == "" { // skip entries with empty user and/or password
			continue
		}

		homedir := filepath.Clean(path.Join(davDir, parts[0]))
		if err := ensureHomeExists(homedir); err != nil {
			return fmt.Errorf("ensure home %q for user %q error: %w", parts[0], homedir, err)
		}

		m.handlers[parts[0]] = newUserHandler(parts[0], parts[1], homedir, backuper)
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
			return fmt.Errorf("make home eror: %w", err)
		}
	default:
		return fmt.Errorf("check home error: %w", err)
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
		return fmt.Errorf("invalid wikis dir %q; error: %w", c.davDir, err)
	}

	if c.authMethod != AuthNone && c.authMethod != AuthBasic {
		return errors.New("invalid auth type") //nolint:err113
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
		return fmt.Errorf("start listen error: %w", err)
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
		return fmt.Errorf("serve http failed: %w", err)
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
		CurvePreferences:         []tls.CurveID{tls.X25519, tls.CurveP521, tls.CurveP384, tls.CurveP256},
		PreferServerCipherSuites: true,
	}

	if err := srv.ServeTLS(lis, c.tlsCert, c.tlsKey); err != nil {
		return fmt.Errorf("serve https failed failed: %w", err)
	}

	return nil
}

func (c *Server) setupHandlers(backuper *Backuper) (http.Handler, []string, error) {
	if c.authMethod == AuthNone {
		return newUserHandler("", "", c.davDir, backuper), nil, nil
	}

	m := &MultiUserHandler{auth: c.authMethod} //nolint:exhaustruct
	if err := m.loadUsers(c.davDir, c.htpassPath, backuper); err != nil {
		return nil, nil, fmt.Errorf("load users error: %w", err)
	}

	return m, slices.Collect(maps.Keys(m.handlers)), nil
}
