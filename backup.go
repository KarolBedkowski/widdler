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
	"slices"
	"sort"
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
}

func (b *Backuper) start() {
	b.enabled = b.keepDaily > 0 || b.keepOnWrite > 0

	if !b.enabled {
		log.Println("Backups disabled")
		return
	}

	log.Printf("Backups enabled; dir: %q; max files: %d on write, %d daily, min age: %ds, compress: %v\n",
		b.backupDir, b.keepOnWrite, b.keepDaily, b.interval, b.compress)

	b.backupsAge = make(map[string]time.Time)

	go b.cleanWorker()
}

func (b *Backuper) create(user, reqPath string) error {
	if !b.enabled {
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

	base, ext := splitNameExt(dstFilePath)
	dstFilename := base + "--" + now.Format("20060102_150405") + ext

	_, err := b.backupFile(srcFilePath, dstFilename)

	return err
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

func (b *Backuper) cleanWorker() {
	usersDirs := make([]string, 0, len(users))
	if len(users) == 0 {
		usersDirs = append(usersDirs, path.Join(davDir, b.backupDir))
	} else {
		for _, u := range users {
			usersDirs = append(usersDirs, path.Join(davDir, u, b.backupDir))
		}
	}

	log.Printf("clean old backups worker started; dirs %v", usersDirs)

	c := time.Tick(cleanTaskInterval * time.Second)
	for {
		for _, ud := range usersDirs {
			if err := b.deleteOldBackups(ud); err != nil {
				log.Printf("delete old backups in %q error: %s\n", ud, err)
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

	toDel := selectFilesToDel(allFiles, time.Now(), b.keepOnWrite, b.keepDaily)

	// delete
	for _, fname := range toDel {
		fmt.Printf("delete old backup: %s\n", fname)
		if err := os.Remove(fname); err != nil {
			return fmt.Errorf("remove %q error: %w", fname, err)
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
	todayFiles := 0
	dailyFiles := 0
	prevDate := ""

	var toDel []string

	for _, f := range files {
		sp := backupNameRe.FindStringSubmatch(f)
		if len(sp) < 4 {
			continue
		}

		date := sp[1]

		if date == today {
			// found today created file; count it and delete if number > keepOnWrite
			todayFiles += 1
			if todayFiles > keepOnWrite {
				toDel = append(toDel, f)
			}

			continue
		}

		// found files older than today

		// more than required files found; delete it
		if dailyFiles >= keepDaily {
			toDel = append(toDel, f)
			continue
		}

		if date == prevDate {
			// found another file in the same date as previous, delete
			toDel = append(toDel, f)
			continue
		}

		prevDate = date
		dailyFiles += 1

		if dailyFiles >= keepDaily {
			toDel = append(toDel, f)
		}
	}

	return toDel
}
