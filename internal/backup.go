package internal

//
// backup.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	slogctx "github.com/veqryn/slog-context"
	"gitlab.com/tozd/go/errors"
)

const cleanTaskInterval = 3600 // sec

const (
	backupModeFile   = "file"
	backupModeSqlite = "sqlite"
)

// ---------------------------------------------------------------------------------

type BackupHandler interface {
	Create(ctx context.Context, root *os.Root, user, srcFilePath string) error
	Clean(ctx context.Context, users []string) error
}

type backupListHandler interface {
	ListHandler(ctx context.Context, w http.ResponseWriter, r *http.Request, root *os.Root, user string) bool
}

// ---------------------------------------------------------------------------------

type Backuper struct {
	handler            BackupHandler
	backupsAge         map[string]time.Time
	cfg                *Config
	enabled            bool
	supportBackupsPage bool

	mu sync.RWMutex
}

func newBackuper(ctx context.Context, cfg *Config) (*Backuper, error) {
	backuper := &Backuper{ //nolint:exhaustruct
		cfg:        cfg,
		backupsAge: make(map[string]time.Time),
	}

	var err error

	switch cfg.BackupMode {
	case backupModeFile:
		backuper.handler, err = newBackuperFile(cfg.WikisDir, cfg.BackupDir, cfg.BackupPolicy, cfg.BackupCompress)
		if err != nil {
			return backuper, errors.WithMessage(err, "prepare file backup")
		}

	case backupModeSqlite:
		backuper.handler, err = newBackuperSqlite(ctx, cfg.BackupSqlite, cfg.BackupPolicy, cfg.BackupCompress)
		if err != nil {
			return backuper, errors.WithMessage(err, "prepare sqlite backup")
		}

	case "":
		slog.InfoContext(ctx, "backups: backups disabled")

		return backuper, nil
	default:
		return backuper, errors.Errorf("unknown backup mode %q", cfg.BackupMode)
	}

	slog.InfoContext(ctx, "backups: backup enabled", "backup_mode", cfg.BackupMode)

	if _, ok := backuper.handler.(backupListHandler); ok {
		backuper.supportBackupsPage = true
	}

	backuper.enabled = true

	return backuper, nil
}

func (b *Backuper) Run(ctx context.Context, users []string) error {
	b.cleanWorker(ctx, users)

	return nil
}

// create backup of `srcFilePath` in `root`.
func (b *Backuper) create(ctx context.Context, root *os.Root, user, srcFilePath string) error {
	if !b.enabled {
		return nil
	}

	ctx = slogctx.Append(ctx, slog.String("file", srcFilePath))

	if _, err := root.Stat(srcFilePath); err != nil {
		if os.IsNotExist(err) {
			slog.WarnContext(ctx, "file to backup not exists")

			return nil
		}

		return errors.WithMessage(err, "stat file")
	}

	filekey := user + "|" + root.Name() + "|" + srcFilePath

	// check is need to create backup for given name; if interval=0 always create backup
	if b.cfg.BackupInterval > 0 && !b.needBackup(filekey) {
		return nil
	}

	if err := b.handler.Create(ctx, root, user, srcFilePath); err != nil {
		return errors.WithMessage(err, "create backup")
	}

	// store backup time
	b.mu.Lock()
	b.backupsAge[filekey] = time.Now()
	b.mu.Unlock()

	return nil
}

func (b *Backuper) needBackup(key string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if oldBackupTs, ok := b.backupsAge[key]; ok {
		// new day, always create backup
		if now := time.Now(); oldBackupTs.YearDay() != now.YearDay() || now.Year() != oldBackupTs.Year() {
			return true
		}

		// skip backup when oldbackup is not older that interval.
		if time.Since(oldBackupTs) < time.Duration(b.cfg.BackupInterval)*time.Second {
			return false
		}
	}

	// backup should be created
	return true
}

func (b *Backuper) cleanWorker(ctx context.Context, users []string) {
	slog.DebugContext(ctx, "backups: clean old backups worker started", "users", users)

	if len(users) == 0 {
		users = []string{""}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.Tick(cleanTaskInterval * time.Second):
			if err := b.handler.Clean(ctx, users); err != nil {
				slog.ErrorContext(ctx, "backups: clean old backups failed", "err", err)
			}
		}
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
