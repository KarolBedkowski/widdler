package main

import (
	"crypto/tls"
	"embed"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
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
	mu   sync.Mutex
	dav  *webdav.Handler
	fs   http.Handler
	user string
	pass string
	home string
}

func newUserHandler(user, pass, homedir string) *userHandler {
	return &userHandler{ //nolint:exhaustruct
		user: user,
		pass: pass,
		home: homedir,
		dav: &webdav.Handler{ //nolint:exhaustruct
			LockSystem: webdav.NewMemLS(),
			FileSystem: webdav.Dir(homedir),
			Logger: func(_ *http.Request, err error) {
				if err != nil {
					log.Print(err)
				}
			},
		},
		fs: http.FileServer(http.Dir(homedir)),
	}
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

// -------------------------------------------------------------------

type userHandlers map[string]*userHandler

// -------------------------------------------------------------------

var (
	auth           string
	davDir         string
	fullListen     string
	genHtpass      bool
	handlers       userHandlers
	defaultHandler *userHandler
	listen         string
	passPath       string
	tlsCert        string
	tlsKey         string
	users          map[string]string
	version        bool
	build          string
)

var pledges = "stdio wpath rpath cpath tty inet dns unveil"

var backuper = Backuper{} //nolint:exhaustruct

const defaultBackupInterval = 60 // sec

func initApp() {
	users = make(map[string]string)

	flag.StringVar(&davDir, "wikis", ".", "Directory of TiddlyWikis to serve over WebDAV.")
	flag.StringVar(&listen, "http", "localhost:8080", "Listen on")
	flag.StringVar(&tlsCert, "tlscert", "", "TLS certificate.")
	flag.StringVar(&tlsKey, "tlskey", "", "TLS key.")
	flag.StringVar(&passPath, "htpass", ".htpasswd", "Path to .htpasswd file..")
	flag.StringVar(&auth, "auth", "none", "Enable HTTP Basic Authentication (basic, none, header).")
	flag.BoolVar(&genHtpass, "gen", false, "Generate a .htpasswd file or add a new entry to an existing file.")
	flag.BoolVar(&version, "v", false, "Show version and exit.")

	flag.StringVar(&backuper.backupDir, "backup.dir", "backups", "Directory for backups in user directory.")
	flag.BoolVar(&backuper.compress, "backup.compress", false, "GZIP backup files.")
	flag.IntVar(&backuper.keepDaily, "backup.keep_daily", 0, "If > 0 keep given number of daily backups.")
	flag.IntVar(&backuper.keepOnWrite, "backup.keep_on_write", 0, "If > 0 keep given number of backup created on write.)")
	flag.IntVar(&backuper.interval, "backup.interval", defaultBackupInterval, "Minimal time between backups (in seconds)")
	flag.Parse()

	// These are OpenBSD specific protections used to prevent unnecessary file access.
	_ = protect.Unveil(passPath, "rwc")
	_ = protect.Unveil(davDir, "rwc")
	_ = protect.Unveil("/etc/ssl/cert.pem", "r")
	_ = protect.Unveil("/etc/resolv.conf", "r")
	_ = protect.Pledge(pledges)

	var err error

	davDir, err = filepath.Abs(davDir)
	if err != nil {
		log.Fatalln(err)
	}

	log.Printf("Wikis directory: %s\n", davDir)
	log.Printf("Auth: %s\n", auth)
}

func logger(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := time.Now()
		fmt.Printf("%s (%s) [%s] \"%s %s\" %03d\n", //nolint:forbidigo
			r.RemoteAddr,
			n.Format(time.RFC822Z),
			r.Method,
			r.URL.Path,
			r.Proto,
			r.ContentLength,
		)
		f(w, r)
	}
}

func createEmpty(path string) error {
	if _, fErr := os.Stat(path); !os.IsNotExist(fErr) {
		return nil
	}

	log.Printf("creating %q\n", path)

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

func mainGenPass() {
	user, err := prompt("Username: ", false)
	if err != nil {
		log.Fatalln(err)
	}

	pass, err := prompt("Password: ", true)
	if err != nil {
		log.Fatalln(err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pass), 11) //nolint:mnd
	if err != nil {
		log.Fatalln(err)
	}

	const filePerm = 0o600

	f, err := os.OpenFile(filepath.Clean(passPath), os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		log.Fatalln(err)
	}

	if _, err := fmt.Fprintf(f, "%s:%s\n", user, hash); err != nil {
		log.Fatalln(err)
	}

	if err = f.Close(); err != nil {
		log.Fatalln(err)
	}

	fmt.Printf("Added %q to %q\n", user, passPath) //nolint:forbidigo
}

func loadUsers() error {
	passPath = filepath.Clean(passPath)

	if _, fErr := os.Stat(passPath); os.IsNotExist(fErr) {
		if auth == AuthBasic || auth == AuthHeader {
			return errors.New("no password file found") //nolint:err113
		}

		return nil
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

	handlers = make(map[string]*userHandler, len(entries))

	for _, parts := range entries {
		uPath := filepath.Clean(path.Join(davDir, parts[0]))

		handlers[parts[0]] = newUserHandler(parts[0], parts[1], uPath)
	}

	return nil
}

func getFirstHeaderByPrefix(h http.Header, prefix string) (string, string) {
	for name, values := range h {
		if strings.HasPrefix(name, prefix) {
			return strings.TrimLeft(name, prefix), values[0]
		}
	}

	return "", ""
}

func handlerForUser(r *http.Request) *userHandler {
	var user, pass string

	switch auth {
	case AuthBasic:
		user, pass, _ = r.BasicAuth()
	case AuthHeader:
		user, pass = getFirstHeaderByPrefix(r.Header, "Auth")
	default:
		return defaultHandler
	}

	if h, ok := handlers[user]; ok && h.authenticate(pass) {
		return h
	}

	return nil
}

func handleHTML(w http.ResponseWriter, r *http.Request, handler *userHandler, fullPath string) bool {
	if ext := filepath.Ext(r.URL.Path); ext != ".html" && ext != ".htm" {
		return false
	}

	if err := createEmpty(fullPath); err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return true
	}

	if r.Method == http.MethodPut {
		if err := backuper.create(handler.user, r.URL.Path); err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return true
		}
	}

	handler.dav.ServeHTTP(w, r)

	return true
}

func handleBrowse(w http.ResponseWriter, r *http.Request, handler *userHandler) bool {
	entries, err := os.ReadDir(handler.home)
	if err != nil {
		log.Println(err)
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
		if _, fErr := os.Stat(path.Join(handler.home, "index.html")); !os.IsNotExist(fErr) {
			http.Redirect(w, r, "/index.html", http.StatusMovedPermanently)

			return true
		}
	}

	handler.fs.ServeHTTP(w, r)

	return true
}

func handleLanding(w http.ResponseWriter, user string) {
	// Landing will be used to fill our landing template
	l := struct {
		User string
		URL  string
	}{user, fullListen + "/wiki.html"}

	templ, err := template.New("landing").Parse(landingPage)
	if err != nil {
		log.Fatalf("parse landing pager error: %s", err)
	}

	if err := templ.ExecuteTemplate(w, "landing", l); err != nil {
		log.Printf("execute template error: %s\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	// Block .htpasswd, Prevent directory traversal
	if strings.Contains(r.URL.Path, ".htpasswd") || strings.Contains(r.URL.Path, "..") {
		http.NotFound(w, r)

		return
	}

	handler := handlerForUser(r)
	if handler == nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="widdler"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)

		return
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	fullPath := handler.resolveFile(r.URL.Path)
	if fullPath == "" {
		http.Error(w, "Bad request", http.StatusBadRequest)

		return
	}

	log.Printf("Resolved file: %s", fullPath)

	if err := handler.ensureHomeExists(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	// HTML files will be created or sent back
	if handleHTML(w, r, handler, fullPath) {
		return
	}

	// Everything else is browsable
	if handleBrowse(w, r, handler) {
		return
	}

	handleLanding(w, handler.user)
}

func main() {
	initApp()

	if version {
		fmt.Println(build) //nolint:forbidigo
		os.Exit(0)
	}

	if genHtpass {
		mainGenPass()
		os.Exit(0)
	}

	pledges, _ = protect.ReducePledges(pledges, "tty")

	// drop to only read on passPath
	_ = protect.Unveil(passPath, "r")
	pledges, _ = protect.ReducePledges(pledges, "unveil")

	if err := loadUsers(); err != nil {
		log.Fatalf("load users error: %s\n", err)
	}

	if auth != "basic" && auth != "header" {
		defaultHandler = newUserHandler("", "", davDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", logger(handler))

	srv := http.Server{ //nolint:exhaustruct
		Handler:           mux,
		ReadHeaderTimeout: 0,
	}

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatalln(err)
	}

	if tlsCert != "" && tlsKey != "" {
		fullListen = "https://" + listen

		srv.TLSConfig = &tls.Config{ //nolint:exhaustruct
			MinVersion:               tls.VersionTLS12,
			CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
			PreferServerCipherSuites: true,
		}

		log.Printf("Listening for HTTPS on 'https://%s'", listen)
		log.Fatalln(srv.ServeTLS(lis, tlsCert, tlsKey))
	}

	backuper.start()

	fullListen = "http://" + listen

	log.Printf("Listening for HTTP on 'http://%s'", listen)
	log.Fatalln(srv.Serve(lis))
}
