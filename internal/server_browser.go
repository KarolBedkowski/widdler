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
	"path/filepath"
	"strings"
)

func (u *userHandler) handleBrowse(w http.ResponseWriter, r *http.Request, reqpath string) error {
	if r.Method == http.MethodPost {
		return u.handleNewFile(w, r, reqpath)
	}

	content, err := u.getDirContent(r.Context(), reqpath)
	if err != nil {
		return fmt.Errorf("read dir %q error: %w", u.home, err)
	}

	w.Header().Add("Cache-Control", "no-cache")

	WritePageTemplate(w, &ServerBrowserIndexPage{
		data: &content,
	})

	return nil
}

func (u *userHandler) handleNewFile(w http.ResponseWriter, r *http.Request, reqpath string) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024) //nolint:mnd // 1k
	filename := r.FormValue("filename")
	filename = filepath.Base(filepath.Clean(filename))

	if !isValidFilename(filename) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Invalid filename"))

		slog.DebugContext(r.Context(), "invalid filename", "filename", filename)

		return nil
	}

	if !strings.HasSuffix(filename, ".html") && !strings.HasSuffix(filename, ".htm") {
		filename += ".html"
	}

	slog.DebugContext(r.Context(), "redirect to", "reqpath", reqpath, "filename", filename)

	http.Redirect(w, r, "/"+reqpath+"/"+filename, http.StatusMovedPermanently)

	return nil
}

func (u *userHandler) getDirContent(ctx context.Context, reqpath string) (DirContent, error) {
	_ = ctx

	rdfs, _ := u.root.FS().(fs.ReadDirFS)

	entries, err := rdfs.ReadDir(reqpath)
	if err != nil {
		return DirContent{}, fmt.Errorf("read dir %q error: %w", u.home, err)
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

	return DirContent{
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
