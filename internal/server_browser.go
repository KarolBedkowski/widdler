package internal

//
// server_browser.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gitlab.com/tozd/go/errors"
)

func (u *userHomeHandler) handleBrowse(w http.ResponseWriter, r *http.Request, root *os.Root, reqpath string) error {
	if r.Method == http.MethodPost {
		return u.handleNewFile(w, r, reqpath)
	}

	content, err := u.getDirContent(root, reqpath)
	if err != nil {
		return WithPublic(errors.WithMessage(err, "read dir content"), "Internal error: can read wikis directory")
	}

	w.Header().Add("Cache-Control", "no-cache")

	WritePageTemplate(w, &ServerBrowserIndexPage{data: content})

	return nil
}

func (u *userHomeHandler) handleNewFile(w http.ResponseWriter, r *http.Request, reqpath string) error {
	if !validateContentType(r, "application/x-www-form-urlencoded") {
		http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)

		return nil
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024) //nolint:mnd // 1k
	filename := r.FormValue("filename")
	filename = filepath.Base(filename)

	if !isValidFilename(filename) {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		slog.DebugContext(r.Context(), "handle new file error - invalid filename", "filename", filename)

		return nil
	}

	if !strings.HasSuffix(filename, ".html") && !strings.HasSuffix(filename, ".htm") {
		filename += ".html"
	}

	slog.DebugContext(r.Context(), "redirect to", "reqpath", reqpath, "filename", filename)
	http.Redirect(w, r, "/"+reqpath+"/"+filename, http.StatusMovedPermanently) //nolint:gosec

	return nil
}

func (u *userHomeHandler) getDirContent(root *os.Root, reqpath string) (*DirContent, error) {
	rdfs, _ := root.FS().(fs.ReadDirFS)

	entries, err := rdfs.ReadDir(reqpath)
	if err != nil {
		return nil, errors.WithMessage(err, "read dir")
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

	return &DirContent{
		Files:             files,
		Dirs:              dirs,
		Parent:            parent,
		Path:              reqpath,
		SupportBackupsDir: u.b.supportBackupsPage,
	}, nil
}

type DirContent struct {
	Files             []fs.DirEntry
	Dirs              []fs.DirEntry
	Parent            string
	Path              string
	SupportBackupsDir bool
}
