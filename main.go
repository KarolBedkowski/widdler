package main

import (
	"compress/bzip2"
	"crypto/tls"
	"embed"
	"encoding/csv"
	"errors"
	"flag"
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
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/webdav"
	"golang.org/x/term"
	"suah.dev/protect"
)

const landingPage = `
<h1>Hello{{if .User}} {{.User}}{{end}}! Welcome to widdler!</h1>

<p>To create a new TiddlyWiki html file, simply append an html file name to the URL in the address bar!</p>

<h3>For example:</h3>

<a href="{{.URL}}">{{.URL}}</a>

<p>This will create a new wiki called "<b>wiki.html</b>"</p>

<p>After creating a wiki, this message will be replaced by a list of your wiki files.</p>
`

const twFile = "empty.html.bz2"

//go:embed empty.html.bz2
var tiddly embed.FS

const (
	AuthBasic  = "basic"
	AuthHeader = "header"
	AuthNone   = ""
)

// -------------------------------------------------------------------

type userHandler struct {
	c    *Configuration
	b    *Backuper
	mu   sync.Mutex
	dav  *webdav.Handler
	fs   http.Handler
	user string
	pass string
	home string
	root *os.Root
}

func newUserHandler(c *Configuration, user, pass, homedir string, backuper *Backuper) *userHandler {
	slog.Debug("new user handler", "user", user, "homedir", homedir)

	root, err := os.OpenRoot(homedir)
	if err != nil {
		slog.Error("open home dir failed", "user", user, "homedir", homedir, "err", err)
		os.Exit(1)
	}

	return &userHandler{ //nolint:exhaustruct
		c:    c,
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
					slog.Error("handle error", "user", user, "req", r.URL, "err", err)
				}
			},
		},
		fs: http.FileServerFS(root.FS()),
	}
}

func (u *userHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()

	fullPath := filepath.Clean(path.Join(".", r.URL.Path))
	if fullPath == "" {
		http.Error(w, "Bad request", http.StatusBadRequest)

		return
	}

	slog.Debug("resolved file", "fullPath", fullPath)

	// HTML files will be created or sent back
	if err := u.handleHTML(w, r, fullPath); err == nil {
		return
	} else if !errors.Is(err, ErrNotFound) {
		slog.Error("handle html error", "path", fullPath, "user", u.user, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	// Everything else is browsable
	if err := u.handleBrowse(w, r); err == nil {
		return
	} else if !errors.Is(err, ErrNotFound) {
		slog.Error("handle browse error", "path", r.URL.Path, "user", u.user, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	if err := u.handleLanding(w); err != nil {
		slog.Error("handle landing error", "user", u.user, "err", err)
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

	_, err := u.root.Stat(fullPath)
	switch {
	case os.IsNotExist(err):
		// file not exists, try create empty
		slog.Info("creating empty wiki", "path", fullPath, "root", u.root, "user", u.user)

		if err := createEmpty(u.root, fullPath); err != nil {
			return fmt.Errorf("create empty wiki error: %w", err)
		}
	case err != nil:
		return fmt.Errorf("check file %q error: %w", fullPath, err)
	default:
		// no error, file exists, make backup on put
		if r.Method == http.MethodPut {
			if err := u.b.create(u.root, fullPath); err != nil {
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
	}{u.user, u.c.fullListen + "/wiki.html"}

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

func createEmpty(root *os.Root, path string) error {
	const filePerm = 0o600

	compressed, _ := tiddly.Open(twFile)
	defer compressed.Close()

	inp := bzip2.NewReader(compressed)

	out, err := root.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return fmt.Errorf("open output file error: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, inp); err != nil { //nolint:gosec
		return fmt.Errorf("write error: %w", err)
	}

	return nil
}

// -------------------------------------------------------------------

type userHandlers map[string]*userHandler

// -------------------------------------------------------------------

var build string

type Configuration struct {
	auth       string
	davDir     string
	listen     string
	passPath   string
	tlsCert    string
	tlsKey     string
	fullListen string
}

func (c *Configuration) validate() error {
	var err error

	c.davDir, err = filepath.Abs(c.davDir)
	if err != nil {
		return fmt.Errorf("check wikis dir %q error: %w", c.davDir, err)
	}

	c.passPath = filepath.Clean(c.passPath)

	if c.tlsCert != "" && c.tlsKey != "" {
		c.fullListen = "https://" + c.listen
	} else {
		c.fullListen = "http://" + c.listen
	}

	return nil
}

const defaultBackupInterval = 60 // sec

func loadConfiguration() (string, *Configuration, *Backuper, error) {
	conf := Configuration{} //nolint:exhaustruct
	backuper := Backuper{}  //nolint:exhaustruct

	var genHtpass, version bool

	flag.StringVar(&conf.davDir, "wikis", ".", "Directory of TiddlyWikis to serve over WebDAV.")
	flag.StringVar(&conf.listen, "http", "localhost:8080", "Listen on")
	flag.StringVar(&conf.tlsCert, "tlscert", "", "TLS certificate.")
	flag.StringVar(&conf.tlsKey, "tlskey", "", "TLS key.")
	flag.StringVar(&conf.passPath, "htpass", ".htpasswd", "Path to .htpasswd file..")
	flag.StringVar(&conf.auth, "auth", "none", "Enable HTTP Basic Authentication (basic, none, header).")
	flag.BoolVar(&genHtpass, "gen", false, "Generate a .htpasswd file or add a new entry to an existing file.")
	flag.BoolVar(&version, "v", false, "Show version and exit.")

	flag.StringVar(&backuper.backupDir, "backup.dir", "backups", "Directory for backups in user directory.")
	flag.BoolVar(&backuper.compress, "backup.compress", false, "GZIP backup files.")
	flag.IntVar(&backuper.keepDaily, "backup.keep_daily", 0, "If > 0 keep given number of daily backups.")
	flag.IntVar(&backuper.keepOnWrite, "backup.keep_on_write", 0, "If > 0 keep given number of backup created on write.)")
	flag.IntVar(&backuper.interval, "backup.interval", defaultBackupInterval, "Minimal time between backups (in seconds)")
	flag.StringVar(&backuper.mode, "backup.mode", "", "Backup mode (file, git, git-once)")

	logLevel := flag.String("log.level", "info",
		"Only log messages with the given severity or above. One of: [debug, info, warn, error]")
	logFormat := flag.String("log.format", "", "Output format of log messages. One of: [logfmt, json]")

	flag.Parse()

	setupLogging(logLevel, logFormat)

	if err := conf.validate(); err != nil {
		return "", nil, nil, err
	}

	slog.Info("Wikis directory: " + conf.davDir)
	slog.Info("Auth: " + conf.auth)

	action := "serve"
	if version {
		action = "version"
	} else if genHtpass {
		action = "genHtpass"
	}

	return action, &conf, &backuper, nil
}

func secure(path ...string) string {
	pledges := "stdio wpath rpath cpath tty inet dns unveil"
	// These are OpenBSD specific protections used to prevent unnecessary file access.
	for _, p := range path {
		_ = protect.Unveil(p, "rwc")
	}

	_ = protect.Unveil("/etc/ssl/cert.pem", "r")
	_ = protect.Unveil("/etc/resolv.conf", "r")
	_ = protect.Pledge(pledges)

	return pledges
}

// -------------------------------------------------------------------

type Logger struct {
	next http.Handler
}

func (l *Logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
			rlog.Error("request error - recovered", "err", err, "dur", time.Since(startTS),
				"stack", string(debug.Stack()))
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("internal server error")) //nolint:errcheck
		} else {
			rlog.Info("request finished", "dur", time.Since(startTS))
		}
	}()

	l.next.ServeHTTP(w, r)
}

// -------------------------------------------------------------------

func prompt(prompt string, secure bool) (string, error) {
	fmt.Print(prompt) //nolint:forbidigo

	var input string

	if secure {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return "", fmt.Errorf("read password error: %w", err)
		}

		input = string(b)
	} else if _, err := fmt.Scanln(&input); err != nil {
		return "", fmt.Errorf("read stdin error: %w", err)
	}

	if input == "" {
		return "", fmt.Errorf("empty %q", prompt) //nolint:err113
	}

	return input, nil
}

func mainGenPass(conf *Configuration) error {
	user, err := prompt("Username: ", false)
	if err != nil {
		return fmt.Errorf("get username error: %w", err)
	}

	pass, err := prompt("Password: ", true)
	if err != nil {
		return fmt.Errorf("get password error: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pass), 11) //nolint:mnd
	if err != nil {
		return fmt.Errorf("hash password error: %w", err)
	}

	const filePerm = 0o600

	f, err := os.OpenFile(conf.passPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("open passfile %q error: %w", conf.passPath, err)
	}

	if _, err := fmt.Fprintf(f, "%s:%s\n", user, hash); err != nil {
		return fmt.Errorf("write to passfile error: %w", err)
	}

	if err = f.Close(); err != nil {
		return fmt.Errorf("close passfile error: %w", err)
	}

	fmt.Printf("Added %q to %q\n", user, conf.passPath) //nolint:forbidigo

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

type MultiUserHandler struct {
	c        *Configuration
	handlers userHandlers
}

func (m *MultiUserHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Block .htpasswd, Prevent directory traversal
	if strings.Contains(r.URL.Path, ".htpasswd") || strings.Contains(r.URL.Path, "..") {
		http.NotFound(w, r)

		return
	}

	var user, pass string

	switch m.c.auth {
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

func (m *MultiUserHandler) loadUsers(backuper *Backuper) error {
	if _, fErr := os.Stat(m.c.passPath); os.IsNotExist(fErr) {
		return errors.New("no password file found") //nolint:err113
	}

	p, err := os.Open(m.c.passPath)
	if err != nil {
		return fmt.Errorf("open users file %q error: %w", m.c.passPath, err)
	}

	defer p.Close()

	ht := csv.NewReader(p)
	ht.Comma = ':'
	ht.Comment = '#'
	ht.TrimLeadingSpace = true

	entries, err := ht.ReadAll()
	if err != nil {
		return fmt.Errorf("read users from %q error: %w", m.c.passPath, err)
	}

	m.handlers = make(map[string]*userHandler, len(entries))

	for _, parts := range entries {
		if parts[0] == "" { // skip entries with empty user
			continue
		}

		homedir := filepath.Clean(path.Join(m.c.davDir, parts[0]))
		if err := ensureHomeExists(homedir); err != nil {
			return fmt.Errorf("ensure home %q for user %q error: %w", parts[0], homedir, err)
		}

		m.handlers[parts[0]] = newUserHandler(m.c, parts[0], parts[1], homedir, backuper)
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

func mainServer(conf *Configuration, backuper *Backuper) {
	var (
		handler http.Handler
		users   []string
	)

	if conf.auth != "basic" && conf.auth != "header" {
		handler = newUserHandler(conf, "", "", conf.davDir, backuper)
	} else {
		m := &MultiUserHandler{c: conf} //nolint:exhaustruct
		if err := m.loadUsers(backuper); err != nil {
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
		ReadHeaderTimeout: 0,
	}

	lis, err := net.Listen("tcp", conf.listen)
	if err != nil {
		slog.Error("start listen error", "err", err)
		os.Exit(1)
	}

	backuper.start(users)

	slog.Info("Listening on '" + conf.fullListen + "'")

	if conf.tlsCert != "" && conf.tlsKey != "" {
		srv.TLSConfig = &tls.Config{ //nolint:exhaustruct
			MinVersion:               tls.VersionTLS12,
			CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
			PreferServerCipherSuites: true,
		}

		err = srv.ServeTLS(lis, conf.tlsCert, conf.tlsKey)
	} else {
		err = srv.Serve(lis)
	}

	if err != nil {
		slog.Error("serve error", "err", err)
	}
}

func main() {
	action, conf, backuper, err := loadConfiguration()
	if err != nil {
		fmt.Println(err.Error()) //nolint:forbidigo
		os.Exit(1)
	}

	pledges := secure(conf.davDir, conf.passPath)

	switch action {
	case "version":
		fmt.Println(build) //nolint:forbidigo
	case "genHtpass":
		if err := mainGenPass(conf); err != nil {
			fmt.Printf("generate password error: %s\n", err) //nolint:forbidigo
			os.Exit(1)
		}
	case "serve":
		pledges, _ = protect.ReducePledges(pledges, "tty")

		// drop to only read on passPath
		_ = protect.Unveil(conf.passPath, "r")
		_, _ = protect.ReducePledges(pledges, "unveil")

		backuper.davDir = conf.davDir

		mainServer(conf, backuper)
	}
}

// -------------------------------------------------------------------

func setupLogging(level, format *string) {
	lev := parseLevel(level)
	opts := &slog.HandlerOptions{Level: lev} //nolint:exhaustruct

	var h slog.Handler

	if format != nil && *format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(h))
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
