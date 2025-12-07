package internal

//
// server_browser.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func (u *userHandler) handleBrowse(w http.ResponseWriter, r *http.Request, reqpath string) error {
	if r.Method == http.MethodPost {
		return u.handleNewFile(w, r, reqpath)
	}

	content, err := u.getDirContent(r.Context(), reqpath)

	switch {
	case err != nil:
		return fmt.Errorf("read dir %q error: %w", u.home, err)
	case len(content.Files) == 0 && len(content.Dirs) == 0:
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

	if err := appTemplates.ExecuteTemplate(w, "list.tmpl", &content); err != nil {
		return fmt.Errorf("execute template error: %w", err)
	}

	return nil
}

func (u *userHandler) handleNewFile(w http.ResponseWriter, r *http.Request, reqpath string) error {
	filename := r.FormValue("filename")
	if filename == "" {
		w.WriteHeader(http.StatusBadRequest)

		return nil
	}

	filename = filepath.Base(filename)
	if !strings.HasSuffix(filename, ".html") && !strings.HasSuffix(filename, ".htm") {
		filename += ".html"
	}

	slog.DebugContext(r.Context(), "redirect to", "reqpath", reqpath, "filename", filename)

	http.Redirect(w, r, "/"+reqpath+"/"+filename, http.StatusMovedPermanently)

	return nil
}

func (u *userHandler) getDirContent(ctx context.Context, reqpath string) (dirContent, error) {
	_ = ctx

	rdfs, _ := u.root.FS().(fs.ReadDirFS)

	entries, err := rdfs.ReadDir(reqpath)
	switch {
	case err != nil:
		return dirContent{}, fmt.Errorf("read dir %q error: %w", u.home, err)
	case len(entries) == 0:
		return dirContent{}, ErrNotFound
	}

	files := make([]fs.DirEntry, 0, len(entries))
	dirs := make([]fs.DirEntry, 0, len(entries))

	for _, e := range entries {
		if e.Name()[0] == '.' || e.Name()[0] == '_' {
			continue
		}

		if e.IsDir() {
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}

	parent := "/"

	if reqpath == "." {
		reqpath = ""
	} else {
		reqpath = "/" + reqpath
		parent = filepath.Dir(reqpath)
	}

	return dirContent{
		Files:             files,
		Dirs:              dirs,
		Parent:            parent,
		Path:              reqpath,
		SupportBackupsDir: u.b.supportBackupsPage,
	}, nil
}

type dirContent struct {
	Files             []fs.DirEntry
	Dirs              []fs.DirEntry
	Parent            string
	Path              string
	SupportBackupsDir bool
}
