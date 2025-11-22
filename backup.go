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
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/urfave/cli/v3"
)

const cleanTaskInterval = 300 // sec

type BackupHandler interface {
	Create(ctx context.Context, root *os.Root, user, srcFilePath string) error
	Clean(ctx context.Context, users []string) error
}

const (
	backupModeFile    = "file"
	backupModeGIT     = "git"
	backupModeGITOnce = "git-once"
	backupModeSqlite  = "sqlite"
)

type Backuper struct {
	enabled    bool
	interval   int
	backupsAge map[string]time.Time
	handler    BackupHandler
	mode       string
}

func newBackuper(ctx context.Context, cmd *cli.Command) (Backuper, error) {
	backupDir := cmd.String("backup.dir")
	davDir := cmd.String("wikis")

	backuper := Backuper{ //nolint:exhaustruct
		interval:   cmd.Int("backup.interval"),
		enabled:    true,
		mode:       cmd.String("backup.mode"),
		backupsAge: make(map[string]time.Time),
	}

	switch backuper.mode {
	case backupModeFile:
		backuper.handler = &BackuperFile{
			baseDir:     davDir,
			backupDir:   backupDir,
			compress:    cmd.Bool("backup.compress"),
			keepOnWrite: cmd.Int("backup.keep_on_write"),
			keepDaily:   cmd.Int("backup.keep_daily"),
		}

	case backupModeSqlite:
		var err error

		backuper.handler, err = newBackuperSqlite(ctx, cmd.String("backup.sqlite_file"))
		if err != nil {
			return backuper, fmt.Errorf("create sqlite backup failed: %w", err)
		}

	case backupModeGIT, backupModeGITOnce:
		backuper.handler = &BackuperGit{}
	case "":
		slog.Info("Backups disabled")

		backuper.enabled = false

		return backuper, nil
	default:
		backuper.enabled = false

		return backuper, fmt.Errorf("unknown backup mode %q", backuper.mode) //nolint:err113
	}

	return backuper, nil
}

func (b *Backuper) start(ctx context.Context, users []string) {
	go b.cleanWorker(ctx, users)
}

func (b *Backuper) create(ctx context.Context, root *os.Root, user, srcFilePath string) error {
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

	if err := b.handler.Create(ctx, root, user, srcFilePath); err != nil {
		return fmt.Errorf("create backup failed: %w", err)
	}

	return nil
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

func (b *Backuper) cleanWorker(ctx context.Context, users []string) {
	slog.DebugContext(ctx, "clean old backups worker started", "users", users)

	c := time.Tick(cleanTaskInterval * time.Second)

	for {
		if err := b.handler.Clean(ctx, users); err != nil {
			slog.ErrorContext(ctx, "clean old backups failed", "err", err)
		}

		<-c
	}
}
