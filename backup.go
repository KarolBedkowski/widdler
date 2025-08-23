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
	"errors"
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

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const cleanTaskInterval = 300 // sec

const (
	backupModeDefault = ""
	backupModeFile    = "file"
	backupModeGIT     = "git"
	backupModeGITOnce = "git-once"
)

type Backuper struct {
	enabled   bool
	compress  bool
	interval  int
	backupDir string
	mode      string

	keepOnWrite int
	keepDaily   int

	backupsAge map[string]time.Time

	davDir string
}

func (b *Backuper) start(users []string) {
	if b.mode == backupModeDefault {
		b.mode = backupModeFile
	}

	if b.mode != backupModeGIT && b.mode != backupModeGITOnce && b.mode != backupModeFile {
		slog.Error(fmt.Sprintf("unknown backup mode %q", b.mode))
	}

	b.enabled = b.keepDaily > 0 || b.keepOnWrite > 0 || b.mode == backupModeGIT || b.mode == backupModeGITOnce

	if !b.enabled {
		slog.Info("Backups disabled")

		return
	}

	slog.Info(fmt.Sprintf(
		"Backups enabled; mode: %q; dir: %q; max files: %d on write, %d daily, min age: %ds, compress: %v",
		b.mode, b.backupDir, b.keepOnWrite, b.keepDaily, b.interval, b.compress))

	b.backupsAge = make(map[string]time.Time)

	if b.mode == backupModeFile {
		go b.cleanWorker(users)
	}
}

func (b *Backuper) create(root *os.Root, srcFilePath string) error {
	if !b.enabled {
		return nil
	}

	if _, err := root.Stat(srcFilePath); err != nil {
		if os.IsNotExist(err) {
			// file not exists
			return nil
		}

		return fmt.Errorf("stat file %q error: %w", srcFilePath, err)
	}

	// check is need to create backup for given name
	if !b.needBackup(srcFilePath) {
		return nil
	}

	defer func() { b.backupsAge[srcFilePath] = time.Now() }()

	switch b.mode {
	case backupModeGIT, backupModeGITOnce:
		return b.createGitBackup(root, srcFilePath)

	default:
		return b.createStdBackup(root, srcFilePath)
	}
}

func (b *Backuper) needBackup(srcFilePath string) bool {
	if oldBackupTs, ok := b.backupsAge[srcFilePath]; ok {
		now := time.Now()

		// new day, always create backup
		if oldBackupTs.YearDay() != now.YearDay() || now.Year() != oldBackupTs.Year() {
			return true
		}

		// in git-once backup mode create only one backup for each file in run
		if b.mode == "git-once" {
			return false
		}

		// for other modes skip backup when oldbackup is not older that interval.
		if b.interval > 0 && time.Since(oldBackupTs) < time.Duration(b.interval)*time.Second {
			return false
		}
	}

	// no backup in current run
	return true
}

func (b *Backuper) createStdBackup(root *os.Root, srcFilePath string) error {
	// create backup dir if not exists
	if err := ensureBackupDirExists(root, b.backupDir); err != nil {
		return err
	}

	dstFilePath := filepath.Clean(path.Join(b.backupDir, srcFilePath))
	// build backup file name - add postfix before extension
	base, ext := splitNameExt(dstFilePath)
	dstFilename := base + "--" + time.Now().Format("20060102_150405") + ext

	return b.backupFile(root, srcFilePath, dstFilename)
}

func (b *Backuper) backupFile(root *os.Root, path, dstFilename string) error {
	if b.compress {
		dstFilename += ".gz"
	}

	if _, err := root.Stat(dstFilename); err == nil {
		// already exists; skip
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat backup destination %q error: %w", dstFilename, err)
	}

	slog.Debug("backup file", "src", path, "dst", dstFilename)

	source, err := root.Open(path)
	if err != nil {
		return fmt.Errorf("open %q for backup error: %w", path, err)
	}
	defer closeFile(source, path)

	var destination io.WriteCloser

	destination, err = root.Create(dstFilename)
	if err != nil {
		return fmt.Errorf("create backup file %q error: %w", dstFilename, err)
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

func (b *Backuper) openOrCreateGitRepo(root *os.Root) (*git.Repository, error) {
	repo, err := git.PlainOpen(root.Name())
	if err == nil {
		return repo, nil
	}

	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("open git repository in %q failed: %w", root.Name(), err)
	}

	repo, err = git.PlainInit(root.Name(), false)
	if err != nil {
		return nil, fmt.Errorf("init git repository in %q failed: %w", root.Name(), err)
	}

	return repo, nil
}

func (b *Backuper) createGitBackup(root *os.Root, srcFilePath string) error {
	r, err := b.openOrCreateGitRepo(root)
	if err != nil {
		return err
	}

	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("open work tress failed: %w", err)
	}

	if _, err = w.Add(srcFilePath); err != nil {
		return fmt.Errorf("add file %q to git repository in %q failed: %w", srcFilePath, root.Name(), err)
	}

	commit, err := w.Commit("backup file "+srcFilePath+" "+time.Now().Format(time.DateTime),
		&git.CommitOptions{ //nolint:exhaustruct
			Author: &object.Signature{
				Name:  "widdler",
				Email: "widdler@no.email",
				When:  time.Now(),
			},
		})

	if errors.Is(err, git.ErrEmptyCommit) {
		slog.Debug("backup skipped; no changes")

		return nil
	}

	if err != nil {
		return fmt.Errorf("commit to git repository %q failed: %w", srcFilePath, err)
	}

	slog.Debug("backup committed", "commit", commit.String())

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

func ensureBackupDirExists(root *os.Root, backupDir string) error {
	const dirPerm = 0o700

	// create backup dir if not exists
	if _, err := root.Stat(backupDir); os.IsNotExist(err) {
		if err := root.Mkdir(backupDir, dirPerm); err != nil {
			return fmt.Errorf("create backup dir %q error: %w", backupDir, err)
		}

		slog.Info(fmt.Sprintf("created backup dir %s in %s", backupDir, root.Name()))
	} else if err != nil {
		return fmt.Errorf("stat backup dir %q error: %w", backupDir, err)
	}

	return nil
}

// closeFile close obj and log error.
func closeFile(obj io.Closer, path string) {
	if err := obj.Close(); err != nil {
		slog.Error("close file error", "path", path, "err", err)
	}
}
