package internal

//
// server_userhome.go
// Copyright (C) 2026 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	slogctx "github.com/veqryn/slog-context"
	"gitlab.com/tozd/go/errors"
	"golang.org/x/net/webdav"
)

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

	if !isValidPath(resolvedPath) {
		renderError(w, http.StatusBadRequest, nil)

		return
	}

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
	// create backup on put html files
	if r.Method == http.MethodPut && htmlRequested(r) {
		if err := u.b.create(ctx, root, u.user, resolvedPath); err != nil {
			return WithPublic(errors.WithMessage(err, "create backup"), "Internal error: failed create backup")
		}
	}

	u.dav.ServeHTTP(w, r)

	return nil
}

func (u *userHomeHandler) handleBrowse(w http.ResponseWriter, r *http.Request, root *os.Root, reqpath string) error {
	switch {
	// handle request post from form; other posts are handled by dav
	case r.Method == http.MethodPost && validateContentType(r, "application/x-www-form-urlencoded"):
		u.handleNewFile(w, r, reqpath)

	// render page with dir content on get; webdav dont use get for dir listing
	case r.Method == http.MethodGet:
		if err := u.handleListDirContent(w, root, reqpath); err != nil {
			return WithPublic(errors.WithMessage(err, "read dir content"), "Internal error: can read wikis directory")
		}

	default:
		u.dav.ServeHTTP(w, r)
	}

	return nil
}

func (u *userHomeHandler) handleNewFile(w http.ResponseWriter, r *http.Request, reqpath string) {
	if u.offline {
		renderError(w, http.StatusNotFound, nil)

		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024) //nolint:mnd // 1k
	filename := r.FormValue("filename")
	filename = filepath.Base(filename)

	if !isValidFilename(filename) {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		slog.DebugContext(r.Context(), "handle new file error - invalid filename", "filename", filename)

		return
	}

	if !strings.HasSuffix(filename, ".html") && !strings.HasSuffix(filename, ".htm") {
		filename += ".html"
	}

	slog.DebugContext(r.Context(), "redirect to", "reqpath", reqpath, "filename", filename)
	http.Redirect(w, r, "/"+reqpath+"/"+filename, http.StatusMovedPermanently) //nolint:gosec
}

func (u *userHomeHandler) handleListDirContent(w http.ResponseWriter, root *os.Root, reqpath string) error {
	rdfs, _ := root.FS().(fs.ReadDirFS)

	entries, err := rdfs.ReadDir(reqpath)
	if err != nil {
		return errors.WithMessage(err, "read dir")
	}

	files := make([]fs.DirEntry, 0, len(entries))
	dirs := make([]fs.DirEntry, 0, len(entries))

	for _, e := range entries {
		if e.Name()[0] == '.' {
			continue
		}

		if e.IsDir() {
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}

	parent := ""

	if reqpath == "." {
		reqpath = ""
	} else {
		reqpath = "/" + reqpath
		parent = filepath.Dir(reqpath)
	}

	data := &DirContent{
		Files:             files,
		Dirs:              dirs,
		Parent:            parent,
		Path:              reqpath,
		SupportBackupsDir: u.b.supportBackupsPage,
		SupportCreateNew:  !u.offline,
	}

	w.Header().Add("Cache-Control", "no-cache")

	WritePageTemplate(w, &ServerBrowserIndexPage{data: data})

	return nil
}

// -------------------------------------------------------------------

type DirContent struct {
	Files             []fs.DirEntry
	Dirs              []fs.DirEntry
	Parent            string
	Path              string
	SupportBackupsDir bool
	SupportCreateNew  bool
}

// -------------------------------------------------------------------

func htmlRequested(r *http.Request) bool {
	ext := filepath.Ext(r.URL.Path)

	return ext == ".html" || ext == ".htm"
}

// -------------------------------------------------------------------

func createEmptyWiki(ctx context.Context, root *os.Root, path string) error {
	// check if destination dir is valid
	dstdir := filepath.Dir(path)
	if s, err := root.Stat(dstdir); err != nil {
		return WithPublic(errors.WithMessage(err, "stat dst dir"), "destination directory not exists")
	} else if !s.IsDir() {
		return WithPublic(errors.WithMessage(err, "stat dst dir"), "destination is not a directory")
	}

	slog.InfoContext(ctx, "server: downloading", "url", emptyURL)

	ctx, cancel := context.WithTimeout(ctx, time.Duration(DownloadWikiTimeoutSec)*time.Second)
	defer cancel()

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
