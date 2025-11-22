package main

import (
	"context"
	"crypto/tls"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
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

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"github.com/rs/xid"
	"github.com/urfave/cli/v3"
	slogctx "github.com/veqryn/slog-context"
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

const emptyURL = "https://tiddlywiki.com/empty.html"

const (
	AuthBasic  = "basic"
	AuthHeader = "header"
	AuthNone   = ""
)

const (
	ServerReadTimeout   = 60 * time.Second
	ServerHeaderTimeout = 10 * time.Second
)

var CtxUserKey = any("ctx_user_key")

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
					slog.ErrorContext(r.Context(), "handle error", "req", r.URL, "err", err)
				}
			},
		},
		fs: http.FileServerFS(root.FS()),
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

type userHandlers map[string]*userHandler

// -------------------------------------------------------------------

var build = "dev"

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

func loadConfiguration(cmd *cli.Command) (*Configuration, *Backuper, error) {
	conf := Configuration{ //nolint:exhaustruct
		davDir:   cmd.String("wikis"),
		listen:   cmd.String("http"),
		tlsCert:  cmd.String("tlscert"),
		tlsKey:   cmd.String("tlskey"),
		passPath: cmd.String("htpass"),
		auth:     cmd.String("auth"),
	}

	backuper := Backuper{ //nolint:exhaustruct
		backupDir:   cmd.String("backup.dir"),
		compress:    cmd.Bool("backup.compress"),
		keepDaily:   cmd.Int("backup.keep_daily"),
		keepOnWrite: cmd.Int("backup.keep_on_write"),
		interval:    cmd.Int("backup.interval"),
		mode:        cmd.String("backup.mode"),
		sqliteFile:  cmd.String("backup.sqliteFile"),
	}

	logLevel := cmd.String("log.level")
	logFormat := cmd.String("log.format")

	setupLogging(&logLevel, &logFormat)

	if err := conf.validate(); err != nil {
		return nil, nil, err
	}

	slog.Info("Wikis directory: " + conf.davDir)
	slog.Info("Auth: " + conf.auth)

	return &conf, &backuper, nil
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
	requestID := xid.New().String()
	ctx := slogctx.Prepend(r.Context(), slog.String("request_id", requestID))

	r = r.WithContext(ctx)

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
			rlog.ErrorContext(ctx, "request error - recovered", "err", err, "dur", time.Since(startTS),
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

func mainGenPass(ctx context.Context, cmd *cli.Command) error {
	_ = ctx
	passPath := cmd.String("htpass")

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

	f, err := os.OpenFile(passPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("open passfile %q error: %w", passPath, err)
	}

	if _, err := fmt.Fprintf(f, "%s:%s\n", user, hash); err != nil {
		return fmt.Errorf("write to passfile error: %w", err)
	}

	if err = f.Close(); err != nil {
		return fmt.Errorf("close passfile error: %w", err)
	}

	fmt.Printf("Added %q to %q\n", user, passPath) //nolint:forbidigo

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

func mainServer(ctx context.Context, conf *Configuration, backuper *Backuper) {
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
		ReadHeaderTimeout: ServerHeaderTimeout,
		ReadTimeout:       ServerReadTimeout,
	}

	lconfig := &net.ListenConfig{} //nolint:exhaustruct

	lis, err := lconfig.Listen(ctx, "tcp", conf.listen)
	if err != nil {
		slog.Error("start listen error", "err", err)
		os.Exit(1)
	}

	backuper.start(ctx, users)

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

func main() { //nolint:funlen
	//nolint:exhaustruct
	cmd := &cli.Command{
		Name:    "widdler",
		Version: build,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "log.level",
				Value: "info",
				Usage: "Only log messages with the given severity or above. One of: [debug, info, warn, error]",
			},
			&cli.StringFlag{
				Name:  "log.format",
				Value: "",
				Usage: "Output format of log messages. One of: [logfmt, json, tint]",
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "serve",
				Usage: "start server",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "wikis", Value: ".", Usage: "Directory of TiddlyWikis to serve over WebDAV."},
					&cli.StringFlag{Name: "http", Value: "localhost:8080", Usage: "Listen on"},
					&cli.StringFlag{Name: "tlscert", Usage: "TLS certificate."},
					&cli.StringFlag{Name: "tlskey", Usage: "TLS key."},
					&cli.StringFlag{Name: "htpass", Value: ".htpasswd", Usage: "Path to .htpasswd file.."},
					&cli.StringFlag{
						Name:  "auth",
						Value: "none",
						Usage: "Enable HTTP Basic Authentication (basic, none, header).",
					},
					&cli.StringFlag{
						Name:  "backup.dir",
						Value: "backups",
						Usage: "Directory for backups in user directory.",
					},
					&cli.BoolFlag{Name: "backup.compress", Value: false, Usage: "GZIP backup files."},
					&cli.IntFlag{
						Name:  "backup.keep_daily",
						Value: 0,
						Usage: "If > 0 keep given number of daily backups.",
					},
					&cli.IntFlag{
						Name:  "backup.keep_on_write",
						Value: 0,
						Usage: "If > 0 keep given number of backup created on write.",
					},
					&cli.IntFlag{
						Name:  "backup.interval",
						Value: 30, //nolint:mnd
						Usage: "Minimal time between backups (in seconds)",
					},
					&cli.StringFlag{Name: "backup.mode", Value: "", Usage: "Backup mode (file, git, git-once)"},
					&cli.StringFlag{
						Name:  "backup.sqliteFile",
						Value: "backup.sqlite",
						Usage: "Backup file for sqlite-mode",
					},
				},
				Action: serverCmd,
			},
			{
				Name: "genHtpass",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "htpass",
						Value:    ".htpasswd",
						Usage:    "Path to .htpasswd file.",
						Required: true,
					},
				},
				Action: mainGenPass,
			},
			{
				Name: "list-backups",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "file",
						Value:    "backup.sqlite",
						Usage:    "Path to backup file.",
						Required: true,
					},
					&cli.StringFlag{Name: "username", Usage: "Username for filter backups"},
				},
				Action: listSqliteBackups,
			},
			{
				Name: "restore-backup",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "file",
						Value:    "backup.sqlite",
						Usage:    "Path to backup file.",
						Required: true,
					},
					&cli.Int64Flag{Name: "backupid", Usage: "Backup ID to restore"},
				},
				Action: restoreSqliteBackups,
			},
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

// -------------------------------------------------------------------

func serverCmd(ctx context.Context, cmd *cli.Command) error {
	conf, backuper, err := loadConfiguration(cmd)
	if err != nil {
		return err
	}

	pledges := secure(conf.davDir, conf.passPath)
	pledges, _ = protect.ReducePledges(pledges, "tty")

	// drop to only read on passPath
	_ = protect.Unveil(conf.passPath, "r")
	_, _ = protect.ReducePledges(pledges, "unveil")

	backuper.davDir = conf.davDir

	mainServer(ctx, conf, backuper)

	return nil
}

// -------------------------------------------------------------------

func listSqliteBackups(ctx context.Context, cmd *cli.Command) error {
	dbfilename := cmd.String("file")
	if dbfilename == "" {
		return errors.New("missing database filename") //nolint:err113
	}

	username := cmd.String("username")

	res, err := ListSqliteBackups(ctx, dbfilename, username)
	if err != nil {
		return err
	}

	for _, r := range res {
		fmt.Println(r.ToString()) //nolint:forbidigo
	}

	return nil
}

// -------------------------------------------------------------------

func restoreSqliteBackups(ctx context.Context, cmd *cli.Command) error {
	dbfilename := cmd.String("file")
	if dbfilename == "" {
		return errors.New("missing database filename") //nolint:err113
	}

	backupid := cmd.Int64("backupid")
	if backupid <= 0 {
		return errors.New("missing or invalid backupid") //nolint:err113
	}

	res, err := RestoreSqliteBackup(ctx, dbfilename, backupid)
	if err != nil {
		return err
	}

	fmt.Println(string(res)) //nolint:forbidigo

	return nil
}

// -------------------------------------------------------------------

func setupLogging(level, format *string) {
	lev := parseLevel(level)
	opts := &slog.HandlerOptions{Level: lev} //nolint:exhaustruct

	logFormat := "logfmt"
	isatty := isatty.IsTerminal(os.Stderr.Fd())

	if format != nil && *format != "" {
		logFormat = *format
	} else if isatty {
		// default for console
		logFormat = "tint"
	}

	var handler slog.Handler

	switch logFormat {
	case "tint":
		handler = tint.NewHandler(os.Stderr, &tint.Options{ //nolint:exhaustruct
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
