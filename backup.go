package main

//
// backup.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

var (
	maskDailyBackups   = regexp.MustCompile(`-[0-9]{8}.html?(\.gz)?$`)
	maskOnWriteBackups = regexp.MustCompile(`-[0-9]{8}_[0-9]{6}.html?(\.gz)?$`)
)

type Backuper struct {
	backupDir             string
	compress              bool
	backupOnWrite         int
	backupOnWriteInterval int
	backupDaily           int

	backupsAge map[string]time.Time
}

func (b *Backuper) init() {
	if b.backupOnWrite > 0 {
		log.Printf("Backups on write enabled; dir: '%s'; max files: %d, min age: %ds, compress: %v\n",
			b.backupDir, b.backupOnWrite, b.backupOnWriteInterval, b.compress)
		b.backupsAge = make(map[string]time.Time)
	} else {
		log.Println("Backups disabled")
	}

	if b.backupDaily > 0 {
		log.Printf("Daily Backups enabled; dir: '%s'; max files: %d, compress: %v\n",
			b.backupDir, b.backupDaily, b.compress)
	}
}

func (b *Backuper) create(user, reqPath string) error {
	if b.backupOnWrite < 1 && b.backupDaily < 1 {
		return nil
	}

	srcFilePath := filepath.Clean(path.Join(davDir, user, reqPath))
	if _, err := os.Stat(srcFilePath); err != nil {
		// file not exists
		return nil
	}

	dstFilePath := path.Join(davDir, user, b.backupDir, reqPath)
	return b.createBackup(srcFilePath, filepath.Clean(dstFilePath))
}

func splitNameExt(path string) (string, string) {
	ext := filepath.Ext(path)
	// path to dst file without extension
	base := path[0 : len(path)-len(ext)]

	return base, ext
}

func (b *Backuper) createBackup(srcFilePath, dstFilePath string) error {
	// create backup dir if not exists
	backupDir, _ := filepath.Split(dstFilePath)
	if _, err := os.Stat(backupDir); os.IsNotExist(err) {
		if err := os.MkdirAll(backupDir, 0o700); err != nil {
			return fmt.Errorf("create backup dir %s error: %w", backupDir, err)
		}
	}

	if b.backupOnWrite > 0 {
		if err := b.createBackupOnWrite(srcFilePath, dstFilePath); err != nil {
			return err
		}
	}

	if b.backupDaily > 0 {
		if err := b.createBackupDaily(srcFilePath, dstFilePath); err != nil {
			return err
		}
	}

	return nil
}

func (b *Backuper) createBackupOnWrite(srcFilePath, dstFilePath string) error {
	now := time.Now()

	if b.backupOnWriteInterval > 0 {
		// check is need to create next backup
		if oldBackupTs, ok := b.backupsAge[srcFilePath]; ok {
			if now.Sub(oldBackupTs) < time.Duration(b.backupOnWriteInterval)*time.Second {
				return nil
			}
		}

		b.backupsAge[srcFilePath] = now
	}

	base, ext := splitNameExt(dstFilePath)
	dstFilename := base + "-" + now.Format("20060102_150405") + ext

	created, err := b.backupFile(srcFilePath, dstFilename)
	if err != nil || !created {
		return err
	}

	if err := b.deleteOldBackups(base, maskOnWriteBackups, b.backupOnWrite); err != nil {
		return err
	}

	return nil
}

func (b *Backuper) createBackupDaily(srcFilePath, dstFilePath string) error {
	base, ext := splitNameExt(dstFilePath)
	dstFilename := base + "-" + time.Now().Format("20060102") + ext

	created, err := b.backupFile(srcFilePath, dstFilename)
	if err != nil || !created {
		return err
	}

	if err := b.deleteOldBackups(base, maskDailyBackups, b.backupDaily); err != nil {
		return err
	}

	return nil
}

// closeFile close obj and log error.
func closeFile(obj io.Closer, msg string, v ...any) {
	if err := obj.Close(); err != nil {
		log.Printf(msg, append(v, err))
	}
}

func (b *Backuper) backupFile(path, dstFilename string) (bool, error) {
	if b.compress {
		dstFilename += ".gz"
	}

	if _, err := os.Stat(dstFilename); err == nil {
		// already exists; skip
		return false, nil
	}

	log.Printf("backup %s -> %s\n", path, dstFilename)

	source, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open %s for backup error: %w", path, err)
	}
	defer closeFile(source, "close %s error: %s", path)

	var destination io.WriteCloser

	destination, err = os.Create(dstFilename)
	if err != nil {
		return false, fmt.Errorf("create backup file %s error: %w", dstFilename, err)
	}
	defer closeFile(destination, "close %s error: %s", dstFilename)

	if b.compress {
		destination, err = gzip.NewWriterLevel(destination, gzip.BestCompression)
		defer closeFile(destination, "close gzip error: %s")

		if err != nil {
			return false, fmt.Errorf("create gzip writer error: %w", err)
		}
	}

	if _, err = io.Copy(destination, source); err != nil {
		return false, fmt.Errorf("create backup file error: %w", err)
	}

	return true, nil
}

func (b *Backuper) deleteOldBackups(prefix string, reMask *regexp.Regexp, maxFiles int) error {
	prefix += "-*.htm*"

	// find all files with prefix
	allFiles, err := filepath.Glob(prefix)
	if err != nil {
		return fmt.Errorf("find old backups (%q) for delete error: %w", prefix, err)
	}

	// filter files by mask (separate daily/onWrite files)
	files := make([]string, 0, len(allFiles))
	for _, f := range allFiles {
		if reMask.Match([]byte(f)) {
			files = append(files, f)
		}
	}

	if len(files) <= maxFiles {
		return nil
	}

	// sort by name = by time
	sort.Strings(files)

	// keep only `maxFiles` newest files
	toDel := files[:len(files)-maxFiles]

	// delete
	for _, fname := range toDel {
		fmt.Printf("delete old backup: %s\n", fname)
		if err := os.Remove(fname); err != nil {
			return fmt.Errorf("remove %q error: %w", fname, err)
		}
	}

	return nil
}
