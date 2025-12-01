package main

import (
	"context"
	"crypto/tls"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"text/template"
	"time"

	"github.com/urfave/cli/v3"
	slogctx "github.com/veqryn/slog-context"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/webdav"
)

const landingPage = `
<h1>Hello{{if .User}} {{.User}}{{end}}! Welcome to widdler!</h1>

<p>To create a new TiddlyWiki html file, simply append an html file name to the URL in the address bar!</p>

<h3>For example:</h3>

<a href="{{.URL}}">{{.URL}}</a>

<p>This will create a new wiki called "<b>wiki.html</b>"</p>

<p>After creating a wiki, this message will be replaced by a list of your wiki files.</p>
`

const emptyURL = "https://tiddlywiki.com/empty.html"

const (
	AuthBasic  = "basic"
	AuthHeader = "header"
	AuthNone   = "none"
)

const (
	ServerReadTimeout   = 60 * time.Second
	ServerHeaderTimeout = 10 * time.Second
)

var CtxUserKey = any("ctx_user_key")

// -------------------------------------------------------------------

type userHandler struct {
	fs         http.Handler
	b          *Backuper
	dav        *webdav.Handler
	root       *os.Root
	user       string
	pass       string
	home       string
	fullListen string
	mu         sync.Mutex
}

func newUserHandler(user, pass, homedir, fullListen string, backuper *Backuper) *userHandler {
	slog.Debug("new user handler", "user", user, "homedir", homedir)

	root, err := os.OpenRoot(homedir)
	if err != nil {
		slog.Error("open home dir failed", "user", user, "homedir", homedir, "err", err)
		os.Exit(1)
	}

	return &userHandler{ //nolint:exhaustruct
		b:    backuper,
		user: user,
		pass: pass,
		home: homedir,
		root: root,
		dav: &webdav.Handler{ //nolint:exhaustruct
			LockSystem: webdav.NewMemLS(),
			FileSystem: webdav.Dir(homedir),
			Logger: func(r *http.Request, err error) {
				if err != nil {
					slog.ErrorContext(r.Context(), "handle error", "req", r.URL, "err", err)
				}
			},
		},
		fs:         http.FileServerFS(root.FS()),
		fullListen: fullListen,
	}
}

func (u *userHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()

	ctx := r.Context()
	ctx = slogctx.Append(ctx, slog.String("user", u.user))
	ctx = context.WithValue(ctx, CtxUserKey, u.user)
	r = r.WithContext(ctx)

	fullPath := filepath.Clean(path.Join(".", r.URL.Path))
	if fullPath == "" {
		http.Error(w, "Bad request", http.StatusBadRequest)

		return
	}

	slog.DebugContext(ctx, "resolved file", "fullPath", fullPath)

	// HTML files will be created or sent back
	if err := u.handleHTML(w, r, fullPath); err == nil {
		return
	} else if !errors.Is(err, ErrNotFound) {
		slog.ErrorContext(ctx, "handle html error", "path", fullPath, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	// Everything else is browsable
	if err := u.handleBrowse(w, r); err == nil {
		return
	} else if !errors.Is(err, ErrNotFound) {
		slog.ErrorContext(ctx, "handle browse error", "path", r.URL.Path, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	if err := u.handleLanding(w); err != nil {
		slog.ErrorContext(ctx, "handle landing error", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (u *userHandler) authenticate(pass string) bool {
	return bcrypt.CompareHashAndPassword([]byte(u.pass), []byte(pass)) == nil
}

var ErrNotFound = errors.New("not found")

func (u *userHandler) handleHTML(w http.ResponseWriter, r *http.Request, fullPath string) error {
	if ext := filepath.Ext(r.URL.Path); ext != ".html" && ext != ".htm" {
		return ErrNotFound
	}

	ctx := r.Context()

	_, err := u.root.Stat(fullPath)
	switch {
	case os.IsNotExist(err):
		// file not exists, try create empty
		slog.InfoContext(ctx, "creating empty wiki", "path", fullPath, "root", u.root)

		if err := createEmpty(ctx, u.root, fullPath); err != nil {
			return fmt.Errorf("create empty wiki error: %w", err)
		}
	case err != nil:
		return fmt.Errorf("check file %q error: %w", fullPath, err)
	default:
		// no error, file exists, make backup on put
		if r.Method == http.MethodPut {
			if err := u.b.create(ctx, u.root, u.user, fullPath); err != nil {
				return fmt.Errorf("create backup error: %w", err)
			}
		}
	}

	u.dav.ServeHTTP(w, r)

	return nil
}

func (u *userHandler) handleBrowse(w http.ResponseWriter, r *http.Request) error {
	rdfs, _ := u.root.FS().(fs.ReadDirFS)

	entries, err := rdfs.ReadDir(".")
	switch {
	case err != nil:
		return fmt.Errorf("read dir %q error: %w", u.home, err)
	case len(entries) == 0:
		return ErrNotFound
	}

	if r.URL.Path == "/" {
		// If we have entries, and are serving up /, check for
		// index.html and redirect to that if it exists. We redirect
		// because net/http handles index.html magically for FileServer
		if _, err := os.Stat(path.Join(u.home, "index.html")); !os.IsNotExist(err) {
			http.Redirect(w, r, "/index.html", http.StatusMovedPermanently)

			return nil
		}
	}

	u.fs.ServeHTTP(w, r)

	return nil
}

func (u *userHandler) handleLanding(w http.ResponseWriter) error {
	// Landing will be used to fill our landing template
	l := struct {
		User string
		URL  string
	}{u.user, u.fullListen + "/wiki.html"}

	templ, err := template.New("landing").Parse(landingPage)
	if err != nil {
		return fmt.Errorf("parse landing pager error: %w", err)
	}

	if err := templ.ExecuteTemplate(w, "landing", l); err != nil {
		return fmt.Errorf("execute template error: %w", err)
	}

	return nil
}

// -------------------------------------------------------------------

func createEmpty(ctx context.Context, root *os.Root, path string) error {
	const filePerm = 0o600

	slog.InfoContext(ctx, "downloading "+emptyURL)

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

	slog.InfoContext(ctx, "create empty wiki completed")

	return nil
}

// -------------------------------------------------------------------

func getFirstHeaderByPrefix(h http.Header, prefix string) (string, string) {
	for name, values := range h {
		if strings.HasPrefix(name, prefix) {
			return strings.TrimLeft(name, prefix), values[0]
		}
	}

	return "", ""
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

	var user, pass string

	switch m.auth {
	case AuthBasic:
		user, pass, _ = r.BasicAuth()
	case AuthHeader:
		user, pass = getFirstHeaderByPrefix(r.Header, "Auth")
	}

	if h, ok := m.handlers[user]; ok && h.authenticate(pass) {
		h.ServeHTTP(w, r)

		return
	}

	w.Header().Set("WWW-Authenticate", `Basic realm="widdler"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

func (m *MultiUserHandler) loadUsers(davDir, passPath, fullListen string, backuper *Backuper) error {
	if _, fErr := os.Stat(passPath); os.IsNotExist(fErr) {
		return errors.New("no password file found") //nolint:err113
	}

	p, err := os.Open(passPath)
	if err != nil {
		return fmt.Errorf("open users file %q error: %w", passPath, err)
	}

	defer p.Close()

	ht := csv.NewReader(p)
	ht.Comma = ':'
	ht.Comment = '#'
	ht.TrimLeadingSpace = true

	entries, err := ht.ReadAll()
	if err != nil {
		return fmt.Errorf("read users from %q error: %w", passPath, err)
	}

	m.handlers = make(map[string]*userHandler, len(entries))

	for _, parts := range entries {
		if parts[0] == "" { // skip entries with empty user
			continue
		}

		homedir := filepath.Clean(path.Join(davDir, parts[0]))
		if err := ensureHomeExists(homedir); err != nil {
			return fmt.Errorf("ensure home %q for user %q error: %w", parts[0], homedir, err)
		}

		m.handlers[parts[0]] = newUserHandler(parts[0], parts[1], homedir, fullListen, backuper)
	}

	return nil
}

// -------------------------------------------------------------------

func ensureHomeExists(home string) error {
	const homeDirPerm = 0o700

	_, err := os.Stat(home)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		if err := os.Mkdir(home, homeDirPerm); err != nil {
			return fmt.Errorf("make home dir %q error: %w", home, err)
		}
	default:
		return fmt.Errorf("check home dir %q error: %w", home, err)
	}

	return nil
}

// -------------------------------------------------------------------

type Server struct {
	auth       string
	davDir     string
	listen     string
	passPath   string
	tlsCert    string
	tlsKey     string
	fullListen string
}

func newServer(cmd *cli.Command) Server {
	auth := cmd.String("auth")
	if auth == "none" {
		auth = AuthNone
	}

	server := Server{ //nolint:exhaustruct
		davDir:   cmd.String("wikis"),
		listen:   cmd.String("http"),
		tlsCert:  cmd.String("tlscert"),
		tlsKey:   cmd.String("tlskey"),
		passPath: filepath.Clean(cmd.String("htpass")),
		auth:     auth,
	}

	if server.tlsCert == "" || server.tlsKey == "" {
		server.fullListen = "http://" + server.listen
	} else {
		server.fullListen = "https://" + server.listen
	}

	return server
}

func (c *Server) Validate() error {
	var err error

	c.davDir, err = filepath.Abs(c.davDir)
	if err != nil {
		return fmt.Errorf("check wikis dir %q error: %w", c.davDir, err)
	}

	if c.auth != AuthNone && c.auth != AuthBasic && c.auth != AuthHeader {
		return errors.New("invalid auth type") //nolint:err113
	}

	return nil
}

func (c *Server) Start(ctx context.Context, backuper *Backuper) error {
	var (
		handler http.Handler
		users   []string
	)

	if c.auth == AuthNone {
		handler = newUserHandler("", "", c.davDir, c.fullListen, backuper)
	} else {
		m := &MultiUserHandler{auth: c.auth} //nolint:exhaustruct
		if err := m.loadUsers(c.davDir, c.passPath, c.fullListen, backuper); err != nil {
			slog.Error("load users error:", "err", err)
			os.Exit(1)
		}

		users = slices.Collect(maps.Keys(m.handlers))
		handler = m
	}

	mux := http.NewServeMux()
	mux.Handle("/", &Logger{handler})

	srv := http.Server{ //nolint:exhaustruct
		Handler:           mux,
		ReadHeaderTimeout: ServerHeaderTimeout,
		ReadTimeout:       ServerReadTimeout,
	}

	lconfig := &net.ListenConfig{} //nolint:exhaustruct

	lis, err := lconfig.Listen(ctx, "tcp", c.listen)
	if err != nil {
		slog.Error("start listen error", "err", err)
		os.Exit(1)
	}

	backuper.start(ctx, users)

	slog.Info("Listening on '" + c.fullListen + "'")

	if c.tlsCert == "" || c.tlsKey == "" {
		if err := srv.Serve(lis); err != nil {
			return fmt.Errorf("serve failed: %w", err)
		}

		return nil
	}

	srv.TLSConfig = &tls.Config{ //nolint:exhaustruct
		MinVersion:               tls.VersionTLS12,
		CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
		PreferServerCipherSuites: true,
	}

	if err := srv.ServeTLS(lis, c.tlsCert, c.tlsKey); err != nil {
		return fmt.Errorf("serve failed failed: %w", err)
	}

	return nil
}
