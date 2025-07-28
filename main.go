package main

import (
	"crypto/tls"
	"embed"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
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

const twFile = "empty.html"

//go:embed empty.html
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
}

func newUserHandler(c *Configuration, user, pass, homedir string, backuper *Backuper) *userHandler {
	return &userHandler{ //nolint:exhaustruct
		c:    c,
		b:    backuper,
		user: user,
		pass: pass,
		home: homedir,
		dav: &webdav.Handler{ //nolint:exhaustruct
			LockSystem: webdav.NewMemLS(),
			FileSystem: webdav.Dir(homedir),
			Logger: func(r *http.Request, err error) {
				if err != nil {
					slog.Error("handle error", "user", user, "req", r.URL, "err", err)
				}
			},
		},
		fs: http.FileServer(http.Dir(homedir)),
	}
}

func (u *userHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()

	fullPath := u.resolveFile(r.URL.Path)
	if fullPath == "" {
		http.Error(w, "Bad request", http.StatusBadRequest)

		return
	}

	slog.Debug("resolved file", "fullPath", fullPath)

	if err := u.ensureHomeExists(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	// HTML files will be created or sent back
	if u.handleHTML(w, r, fullPath) {
		return
	}

	// Everything else is browsable
	if u.handleBrowse(w, r) {
		return
	}

	u.handleLanding(w)
}

func (u *userHandler) authenticate(pass string) bool {
	return bcrypt.CompareHashAndPassword([]byte(u.pass), []byte(pass)) == nil
}

func (u *userHandler) resolveFile(file string) string {
	fullPath := filepath.Clean(path.Join(u.home, file))
	if !strings.HasPrefix(fullPath, u.home) {
		return ""
	}

	return fullPath
}

func (u *userHandler) ensureHomeExists() error {
	const homeDirPerm = 0o700

	if _, err := os.Stat(u.home); os.IsNotExist(err) {
		if err := os.Mkdir(u.home, homeDirPerm); err != nil {
			return fmt.Errorf("mkdir %s error: %w", u.home, err)
		}
	}

	return nil
}

func (u *userHandler) handleHTML(w http.ResponseWriter, r *http.Request, fullPath string) bool {
	if ext := filepath.Ext(r.URL.Path); ext != ".html" && ext != ".htm" {
		return false
	}

	if err := createEmpty(fullPath); err != nil {
		slog.Error("create empty wiki error", "path", fullPath, "user", u.user, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return true
	}

	if r.Method == http.MethodPut {
		if err := u.b.create(u.user, r.URL.Path); err != nil {
			slog.Error("create backup errror", "req_path", r.URL.Path, "user", u.user, "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return true
		}
	}

	u.dav.ServeHTTP(w, r)

	return true
}

func (u *userHandler) handleBrowse(w http.ResponseWriter, r *http.Request) bool {
	entries, err := os.ReadDir(u.home)
	if err != nil {
		slog.Error("read dir error", "dir", u.home, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return true
	}

	if len(entries) == 0 {
		return false
	}

	if r.URL.Path == "/" {
		// If we have entries, and are serving up /, check for
		// index.html and redirect to that if it exists. We redirect
		// because net/http handles index.html magically for FileServer
		if _, fErr := os.Stat(path.Join(u.home, "index.html")); !os.IsNotExist(fErr) {
			http.Redirect(w, r, "/index.html", http.StatusMovedPermanently)

			return true
		}
	}

	u.fs.ServeHTTP(w, r)

	return true
}

func (u *userHandler) handleLanding(w http.ResponseWriter) {
	// Landing will be used to fill our landing template
	l := struct {
		User string
		URL  string
	}{u.user, u.c.fullListen + "/wiki.html"}

	templ, err := template.New("landing").Parse(landingPage)
	if err != nil {
		slog.Error("parse landing pager error", "err", err)
		os.Exit(1)
	}

	if err := templ.ExecuteTemplate(w, "landing", l); err != nil {
		slog.Error("execute template error", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
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

type Logger struct {
	next http.Handler
}

func (l *Logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := time.Now()
	fmt.Printf("%s (%s) [%s] \"%s %s\" %03d\n", //nolint:forbidigo
		r.RemoteAddr,
		n.Format(time.RFC822Z),
		r.Method,
		r.URL.Path,
		r.Proto,
		r.ContentLength,
	)

	l.next.ServeHTTP(w, r)
}

func createEmpty(path string) error {
	if _, fErr := os.Stat(path); !os.IsNotExist(fErr) {
		return nil
	}

	slog.Info("creating empty wiki", "path", path)

	const filePerm = 0o600

	twData, _ := tiddly.ReadFile(twFile)
	if err := os.WriteFile(path, twData, filePerm); err != nil {
		return fmt.Errorf("write file %q error: %w", path, err)
	}

	return nil
}

func prompt(prompt string, secure bool) (string, error) {
	fmt.Print(prompt) //nolint:forbidigo

	if secure {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return "", fmt.Errorf("read password error: %w", err)
		}

		return string(b), nil
	}

	var input string
	if _, err := fmt.Scanln(&input); err != nil {
		return "", fmt.Errorf("read stdin error: %w", err)
	}

	return input, nil
}

func mainGenPass(conf *Configuration) error {
	user, err := prompt("Username: ", false)
	if err != nil {
		return fmt.Errorf("get username error: %w", err)
	}

	if user == "" {
		return errors.New("empty user") //nolint:err113
	}

	pass, err := prompt("Password: ", true)
	if err != nil {
		return fmt.Errorf("get password error: %w", err)
	}

	if pass == "" {
		return errors.New("empty password") //nolint:err113
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pass), 11) //nolint:mnd
	if err != nil {
		return fmt.Errorf("hash password error: %w", err)
	}

	const filePerm = 0o600

	f, err := os.OpenFile(filepath.Clean(conf.passPath), os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
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
	b        *Backuper
	handlers userHandlers
}

func (m *MultiUserHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Block .htpasswd, Prevent directory traversal
	if strings.Contains(r.URL.Path, ".htpasswd") || strings.Contains(r.URL.Path, "..") {
		http.NotFound(w, r)

		return
	}

	h := m.handlerForUser(r)
	if h == nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="widdler"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)

		return
	}

	h.ServeHTTP(w, r)
}

func (m *MultiUserHandler) loadUsers() error {
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
		uPath := filepath.Clean(path.Join(m.c.davDir, parts[0]))

		m.handlers[parts[0]] = newUserHandler(m.c, parts[0], parts[1], uPath, m.b)
	}

	return nil
}

func (m *MultiUserHandler) handlerForUser(r *http.Request) *userHandler {
	var user, pass string

	switch m.c.auth {
	case AuthBasic:
		user, pass, _ = r.BasicAuth()
	case AuthHeader:
		user, pass = getFirstHeaderByPrefix(r.Header, "Auth")
	}

	if h, ok := m.handlers[user]; ok && h.authenticate(pass) {
		return h
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
		m := &MultiUserHandler{c: conf, b: backuper} //nolint:exhaustruct
		if err := m.loadUsers(); err != nil {
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

		if err := srv.ServeTLS(lis, conf.tlsCert, conf.tlsKey); err != nil {
			slog.Error("serve error", "err", err)
		}
	}

	if err := srv.Serve(lis); err != nil {
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
