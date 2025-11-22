//go:build !gitint

package main

//
// backup_got_ext.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//
import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

var ErrInvalidGitDir = errors.New(".git is not a directory")

type BackuperGit struct{}

var _ BackupHandler = &BackuperGit{}

func (b *BackuperGit) Create(ctx context.Context, root *os.Root, username, srcFilePath string) error {
	slog.DebugContext(ctx, "backup file start (external)", "file", srcFilePath)

	err := b.openOrCreateGitRepo(ctx, root)
	if err != nil {
		return err
	}

	worktree := root.Name()

	slog.DebugContext(ctx, "backup - add file")

	// no cancel on request cancel
	ctx = context.WithoutCancel(ctx)

	cmd := exec.CommandContext(ctx, "git", "add", "--", srcFilePath)
	cmd.Dir = worktree

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("add file %q to git index error: %w", srcFilePath, err)
	}

	slog.DebugContext(ctx, "backup - diff")

	cmd = exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet")
	cmd.Dir = worktree

	if err := cmd.Run(); err == nil {
		slog.DebugContext(ctx, "backup skipped; no changes")

		return nil
	}

	slog.DebugContext(ctx, "backup - commit")

	msg := "backup file " + srcFilePath + " " + time.Now().Format(time.DateTime)
	cmd = exec.CommandContext(ctx, "git", "commit", "-m", msg)
	cmd.Dir = worktree

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("commit error: %w", err)
	}

	slog.DebugContext(ctx, "backup committed")

	return nil
}

func (b BackuperGit) Clean(ctx context.Context, users []string) error { //nolint:revive
	return nil
}

func (b *BackuperGit) openOrCreateGitRepo(ctx context.Context, root *os.Root) error {
	st, err := root.Stat(".git")

	switch {
	case errors.Is(err, fs.ErrNotExist):
		// not exists, create below
	case err != nil:
		return fmt.Errorf("get stat of .git directory in %q error: %w", root.Name(), err)
	case !st.IsDir():
		return ErrInvalidGitDir
	default:
		// dir exists
		return nil
	}

	worktree := root.Name()

	slog.InfoContext(ctx, "create new git repository in "+worktree)

	// no cancel on request cancel
	ctx = context.WithoutCancel(ctx)

	// not exists, create
	cmd := exec.CommandContext(ctx, "git", "init", worktree)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("can't initiate git repository in %q: %w", worktree, err)
	}

	return nil
}
