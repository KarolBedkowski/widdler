package main

import (
	"crypto/tls"
	"embed"
	"encoding/csv"
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

// Landing will be used to fill our landing template
type Landing struct {
	User string
	URL  string
}

const landingPage = `
<h1>Hello{{if .User}} {{.User}}{{end}}! Welcome to widdler!</h1>

<p>To create a new TiddlyWiki html file, simply append an html file name to the URL in the address bar!</p>

<h3>For example:</h3>

<a href="{{.URL}}">{{.URL}}</a>

<p>This will create a new wiki called "<b>wiki.html</b>"</p>

<p>After creating a wiki, this message will be replaced by a list of your wiki files.</p>
`

var (
	twFile = "empty.html"

	//go:embed empty.html
	tiddly embed.FS
	templ  *template.Template
)

type userHandler struct {
	mu   sync.Mutex
	dav  *webdav.Handler
	fs   http.Handler
	user string
	pass string
	home string
}

func newUserHandler(user, pass, homedir string) *userHandler {
	return &userHandler{
		user: user,
		pass: pass,
		home: homedir,
		dav: &webdav.Handler{
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
	if _, err := os.Stat(u.home); os.IsNotExist(err) {
		if err := os.Mkdir(u.home, 0o700); err != nil {
			return err
		}
	}

	return nil
}

type userHandlers map[string]*userHandler

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

var backuper = Backuper{}

func initApp() {
	users = make(map[string]string)
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatalln(err)
	}

	flag.StringVar(&davDir, "wikis", dir, "Directory of TiddlyWikis to serve over WebDAV.")
	flag.StringVar(&listen, "http", "localhost:8080", "Listen on")
	flag.StringVar(&tlsCert, "tlscert", "", "TLS certificate.")
	flag.StringVar(&tlsKey, "tlskey", "", "TLS key.")
	flag.StringVar(&passPath, "htpass", fmt.Sprintf("%s/.htpasswd", dir), "Path to .htpasswd file..")
	flag.StringVar(&auth, "auth", "none", "Enable HTTP Basic Authentication (basic, none, header).")
	flag.BoolVar(&genHtpass, "gen", false, "Generate a .htpasswd file or add a new entry to an existing file.")
	flag.BoolVar(&version, "v", false, "Show version and exit.")

	flag.StringVar(&backuper.backupDir, "backup.dir", "backups", "Directory for backups in user directory.")
	flag.BoolVar(&backuper.compress, "backup.compress", false, "GZIP backup files.")
	flag.IntVar(&backuper.keepDaily, "backup.keep_daily", 0, "If > 0 keep given number of daily backups.")
	flag.IntVar(&backuper.keepOnWrite, "backup.keep_on_write", 0, "If > 0 keep given number of backup created on write.)")
	flag.IntVar(&backuper.interval, "backup.interval", 60, "Minimal time between backups (in seconds)")
	flag.Parse()

	// These are OpenBSD specific protections used to prevent unnecessary file access.
	_ = protect.Unveil(passPath, "rwc")
	_ = protect.Unveil(davDir, "rwc")
	_ = protect.Unveil("/etc/ssl/cert.pem", "r")
	_ = protect.Unveil("/etc/resolv.conf", "r")
	_ = protect.Pledge(pledges)

	templ, err = template.New("landing").Parse(landingPage)
	if err != nil {
		log.Fatalln(err)
	}

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
		fmt.Printf("%s (%s) [%s] \"%s %s\" %03d\n",
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
	twData, _ := tiddly.ReadFile(twFile)
	if wErr := os.WriteFile(path, twData, 0o600); wErr != nil {
		return wErr
	}

	return nil
}

func prompt(prompt string, secure bool) (string, error) {
	fmt.Print(prompt)

	if secure {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return "", err
		}

		return string(b), nil
	}

	var input string
	if _, err := fmt.Scanln(&input); err != nil {
		return "", err
	}

	return "", nil
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

	hash, err := bcrypt.GenerateFromPassword([]byte(pass), 11)
	if err != nil {
		log.Fatalln(err)
	}

	f, err := os.OpenFile(filepath.Clean(passPath), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Fatalln(err)
	}

	if _, err := fmt.Fprintf(f, "%s:%s\n", user, hash); err != nil {
		log.Fatalln(err)
	}

	if err = f.Close(); err != nil {
		log.Fatalln(err)
	}

	fmt.Printf("Added %q to %q\n", user, passPath)
}

func loadUsers() {
	if _, fErr := os.Stat(passPath); os.IsNotExist(fErr) {
		if auth == "basic" || auth == "header" {
			fmt.Println("No .htpasswd file found!")
			os.Exit(1)
		}

		return
	}

	p, err := os.Open(filepath.Clean(passPath))
	if err != nil {
		log.Fatal(err)
	}

	ht := csv.NewReader(p)
	ht.Comma = ':'
	ht.Comment = '#'
	ht.TrimLeadingSpace = true

	entries, err := ht.ReadAll()
	if err != nil {
		log.Fatal(err)
	}

	if err := p.Close(); err != nil {
		log.Fatal(err)
	}

	handlers = make(map[string]*userHandler, len(entries))

	for _, parts := range entries {
		uPath := path.Join(davDir, parts[0])

		handlers[parts[0]] = newUserHandler(parts[0], parts[1], uPath)
	}
}

func handlerForUser(r *http.Request) *userHandler {
	if auth == "basic" {
		user, pass, _ := r.BasicAuth()
		if h, ok := handlers[user]; ok && h.authenticate(pass) {
			return h
		}

		return nil
	}

	if auth == "header" {
		const prefix = "Auth"

		for name, values := range r.Header {
			if !strings.HasPrefix(name, prefix) {
				continue
			}

			user := strings.TrimLeft(name, prefix)
			if h, ok := handlers[user]; ok && h.authenticate(values[0]) {
				return h
			}

			return nil
		}

		return nil
	}

	return defaultHandler
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

	if r.Method == "PUT" {
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
		if _, fErr := os.Stat(filepath.Clean(path.Join(handler.home, "index.html"))); !os.IsNotExist(fErr) {
			http.Redirect(w, r, "/index.html", http.StatusMovedPermanently)
			return true
		}
	}

	handler.fs.ServeHTTP(w, r)
	return true
}

func handleLanding(w http.ResponseWriter, user string) {
	l := Landing{URL: fmt.Sprintf("%s/wiki.html", fullListen)}

	if user != "" {
		l.User = user
	}

	if err := templ.ExecuteTemplate(w, "landing", l); err != nil {
		log.Println(err)
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
		fmt.Println(build)
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

	loadUsers()

	if auth != "basic" && auth != "header" {
		defaultHandler = newUserHandler("", "", davDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", logger(handler))

	s := http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 0,
	}

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatalln(err)
	}

	if tlsCert != "" && tlsKey != "" {
		fullListen = fmt.Sprintf("https://%s", listen)

		s.TLSConfig = &tls.Config{
			MinVersion:               tls.VersionTLS12,
			CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
			PreferServerCipherSuites: true,
		}

		log.Printf("Listening for HTTPS on 'https://%s'", listen)
		log.Fatalln(s.ServeTLS(lis, tlsCert, tlsKey))
	}

	backuper.start()

	fullListen = fmt.Sprintf("http://%s", listen)

	log.Printf("Listening for HTTP on 'http://%s'", listen)
	log.Fatalln(s.Serve(lis))
}
