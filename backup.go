package main

//
// backup.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//
// Backups are:
// - up to `keepOnWrite` backups created today for each file.
// - up to `keepDaily` last backups created in previous days.

import (
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const cleanTaskInterval = 300 // sec

type Backuper struct {
	enabled   bool
	compress  bool
	interval  int
	backupDir string

	keepOnWrite int
	keepDaily   int

	backupsAge map[string]time.Time

	davDir string
}

func (b *Backuper) start(users []string) {
	b.enabled = b.keepDaily > 0 || b.keepOnWrite > 0

	if !b.enabled {
		slog.Info("Backups disabled")

		return
	}

	slog.Info(fmt.Sprintf("Backups enabled; dir: %q; max files: %d on write, %d daily, min age: %ds, compress: %v",
		b.backupDir, b.keepOnWrite, b.keepDaily, b.interval, b.compress))

	b.backupsAge = make(map[string]time.Time)

	go b.cleanWorker(users)
}

func (b *Backuper) create(user, reqPath string) error {
	if !b.enabled {
		return nil
	}

	srcFilePath := filepath.Clean(path.Join(b.davDir, user, reqPath))
	if _, err := os.Stat(srcFilePath); err != nil {
		if os.IsNotExist(err) {
			// file not exists
			return nil
		}

		return fmt.Errorf("stat file %q error: %w", srcFilePath, err)
	}

	dstFilePath := filepath.Clean(path.Join(b.davDir, user, b.backupDir, reqPath))

	// create backup dir if not exists
	if err := ensureBackupDirExists(dstFilePath); err != nil {
		return err
	}

	now := time.Now()

	if b.interval > 0 {
		// check is need to create next backup
		if oldBackupTs, ok := b.backupsAge[srcFilePath]; ok {
			if now.Sub(oldBackupTs) < time.Duration(b.interval)*time.Second {
				return nil
			}
		}

		b.backupsAge[srcFilePath] = now
	}

	// build backup file name - add postfix before extension
	base, ext := splitNameExt(dstFilePath)
	dstFilename := base + "--" + now.Format("20060102_150405") + ext

	return b.backupFile(srcFilePath, dstFilename)
}

func (b *Backuper) backupFile(path, dstFilename string) error {
	if b.compress {
		dstFilename += ".gz"
	}

	if _, err := os.Stat(dstFilename); err == nil {
		// already exists; skip
		return nil
	}

	slog.Debug("backup file", "src", path, "dst", dstFilename)

	source, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s for backup error: %w", path, err)
	}
	defer closeFile(source, path)

	var destination io.WriteCloser

	destination, err = os.Create(dstFilename)
	if err != nil {
		return fmt.Errorf("create backup file %s error: %w", dstFilename, err)
	}
	defer closeFile(destination, dstFilename)

	if b.compress {
		destination, err = gzip.NewWriterLevel(destination, gzip.BestCompression)
		defer closeFile(destination, "gzip")

		if err != nil {
			return fmt.Errorf("create gzip writer error: %w", err)
		}
	}

	if _, err = io.Copy(destination, source); err != nil {
		return fmt.Errorf("create backup file error: %w", err)
	}

	return nil
}

func (b *Backuper) cleanWorker(users []string) {
	usersDirs := make([]string, 0, len(users))
	if len(users) == 0 {
		usersDirs = append(usersDirs, path.Join(b.davDir, b.backupDir))
	} else {
		for _, u := range users {
			usersDirs = append(usersDirs, path.Join(b.davDir, u, b.backupDir))
		}
	}

	slog.Debug("clean old backups worker started", "dirs", usersDirs)

	c := time.Tick(cleanTaskInterval * time.Second)

	for {
		for _, ud := range usersDirs {
			if err := b.deleteOldBackups(ud); err != nil {
				slog.Error("delete old backups error", "path", ud, "err", err)
			}
		}

		<-c
	}
}

func (b *Backuper) deleteOldBackups(directory string) error {
	prefix := path.Join(directory, "*--*.htm*")

	// find all files with prefix
	allFiles, err := filepath.Glob(prefix)
	if err != nil {
		return fmt.Errorf("find old backups (%q) for delete error: %w", prefix, err)
	}

	groups := groupFilesByPrefix(allFiles)

	for _, files := range groups {
		toDel := selectFilesToDel(files, time.Now(), b.keepOnWrite, b.keepDaily)
		// delete
		for _, fname := range toDel {
			slog.Debug("delete old backup", "path", fname)

			if err := os.Remove(fname); err != nil {
				return fmt.Errorf("remove %q error: %w", fname, err)
			}
		}
	}

	return nil
}

var backupNameRe = regexp.MustCompile(`--([0-9]{8})(_([0-9]{6}))?.html?(\.gz)?$`)

func selectFilesToDel(files []string, now time.Time, keepOnWrite, keepDaily int) []string {
	// sort files from newer to oldest
	sort.Strings(files)
	slices.Reverse(files)

	today := now.Format("20060102")
	prevDate := ""
	toKeep := make([]string, 0, len(files))

	for _, file := range files {
		sp := backupNameRe.FindStringSubmatch(file)
		if len(sp) < 4 { //nolint:mnd
			continue
		}

		dateFromFile := sp[1]

		if dateFromFile == today {
			if keepOnWrite > 0 {
				toKeep = append(toKeep, file)
				keepOnWrite--
			}
		} else { // daily files
			if keepDaily == 0 {
				break
			}

			// keep only one file from each day
			if dateFromFile != prevDate {
				toKeep = append(toKeep, file)
				keepDaily--
				prevDate = dateFromFile
			}
		}
	}

	var toDel []string

	for _, f := range files {
		if !slices.Contains(toKeep, f) {
			toDel = append(toDel, f)
		}
	}

	return toDel
}

func groupFilesByPrefix(files []string) map[string][]string {
	groups := make(map[string][]string, len(files))
	for _, f := range files {
		base := filepath.Base(f)

		prefix, rest, found := strings.Cut(base, "--")
		if !found || rest == "" || prefix == "" {
			continue
		}

		if gr, ok := groups[prefix]; ok {
			groups[prefix] = append(gr, f)
		} else {
			groups[prefix] = []string{f}
		}
	}

	return groups
}

func splitNameExt(path string) (string, string) {
	ext := filepath.Ext(path)
	// path to dst file without extension
	base := path[0 : len(path)-len(ext)]

	return base, ext
}

func ensureBackupDirExists(dstFilePath string) error {
	const dirPerm = 0o700

	// create backup dir if not exists
	backupDir, _ := filepath.Split(dstFilePath)
	if _, err := os.Stat(backupDir); os.IsNotExist(err) {
		if err := os.MkdirAll(backupDir, dirPerm); err != nil {
			return fmt.Errorf("create backup dir %s error: %w", backupDir, err)
		}
	}

	return nil
}

// closeFile close obj and log error.
func closeFile(obj io.Closer, path string) {
	if err := obj.Close(); err != nil {
		slog.Error("close file error", "path", path, "err", err)
	}
}
