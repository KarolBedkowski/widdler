package internal

//
// backup.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/urfave/cli/v3"
	slogctx "github.com/veqryn/slog-context"
)

const cleanTaskInterval = 3600 // sec

type BackupHandler interface {
	Create(ctx context.Context, root *os.Root, user, srcFilePath string) error
	Clean(ctx context.Context, users []string) error
}

const (
	backupModeFile   = "file"
	backupModeSqlite = "sqlite"
)

type Backuper struct {
	handler            BackupHandler
	backupsAge         map[string]time.Time
	mode               string
	interval           int
	enabled            bool
	supportBackupsPage bool
}

func newBackuper(ctx context.Context, cmd *cli.Command) (Backuper, error) {
	backuper := Backuper{ //nolint:exhaustruct
		interval:   cmd.Int("backup.interval"),
		mode:       cmd.String("backup.mode"),
		backupsAge: make(map[string]time.Time),
	}

	var err error

	switch backuper.mode {
	case backupModeFile:
		backuper.handler, err = newBackuperFile(cmd.String("wikis"), cmd.String("backup.dir"),
			cmd.String("backup.policy"), cmd.Bool("backup.compress"))
		if err != nil {
			return backuper, fmt.Errorf("prepare file backup failed: %w", err)
		}

	case backupModeSqlite:
		backuper.handler, err = newBackuperSqlite(ctx, cmd.String("backup.sqlite_file"),
			cmd.String("backup.policy"), cmd.Bool("backup.compress"))
		if err != nil {
			return backuper, fmt.Errorf("create sqlite backup failed: %w", err)
		}

	case "":
		slog.InfoContext(ctx, "backups: backups disabled")

		return backuper, nil
	default:
		return backuper, fmt.Errorf("unknown backup mode %q", backuper.mode) //nolint:err113
	}

	slog.InfoContext(ctx, "backups: backup enabled", "backup_mode", backuper.mode)

	if _, ok := backuper.handler.(backupListHandler); ok {
		backuper.supportBackupsPage = true
	}

	backuper.enabled = true

	return backuper, nil
}

func (b *Backuper) start(ctx context.Context, users []string) {
	if b.enabled {
		go b.cleanWorker(ctx, users)
	}
}

// create backup of `srcFilePath` in `root`.
func (b *Backuper) create(ctx context.Context, root *os.Root, user, srcFilePath string) error {
	if !b.enabled {
		return nil
	}

	ctx = slogctx.With(ctx, slog.String("file", srcFilePath))

	if _, err := root.Stat(srcFilePath); err != nil {
		if os.IsNotExist(err) {
			slog.WarnContext(ctx, "file to backup not exists")

			return nil
		}

		return fmt.Errorf("stat file %q error: %w", srcFilePath, err)
	}

	filekey := user + "|" + root.Name() + "|" + srcFilePath

	// check is need to create backup for given name; if interval=0 always create backup
	if b.interval > 0 && !b.needBackup(filekey) {
		return nil
	}

	// store backup time
	defer func() { b.backupsAge[filekey] = time.Now() }()

	if err := b.handler.Create(ctx, root, user, srcFilePath); err != nil {
		return fmt.Errorf("create backup failed: %w", err)
	}

	return nil
}

func (b *Backuper) needBackup(key string) bool {
	if oldBackupTs, ok := b.backupsAge[key]; ok {
		// new day, always create backup
		if now := time.Now(); oldBackupTs.YearDay() != now.YearDay() || now.Year() != oldBackupTs.Year() {
			return true
		}

		// for other modes skip backup when oldbackup is not older that interval.
		if time.Since(oldBackupTs) < time.Duration(b.interval)*time.Second {
			return false
		}
	}

	// no backup in current run
	return true
}

func (b *Backuper) cleanWorker(ctx context.Context, users []string) {
	slog.DebugContext(ctx, "backups: clean old backups worker started", "users", users)

	if len(users) == 0 {
		users = []string{""}
	}

	for c := time.Tick(cleanTaskInterval * time.Second); ; {
		if err := b.handler.Clean(ctx, users); err != nil {
			slog.ErrorContext(ctx, "backups: clean old backups failed", "err", err)
		}

		<-c
	}
}

func (b *Backuper) handleBackupsPage(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	root *os.Root,
	user string,
) bool {
	if !b.enabled {
		return false
	}

	if bh, ok := b.handler.(backupListHandler); ok {
		return bh.ListHandler(ctx, w, r, root, user)
	}

	return false
}

type backupListHandler interface {
	ListHandler(ctx context.Context, w http.ResponseWriter, r *http.Request, root *os.Root, user string) bool
}
