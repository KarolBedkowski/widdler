//go:build !gitint
// +build !gitint

package main

//
// backup_got_ext.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//
import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

var ErrInvalidGitDir = errors.New(".git is not a directory")

func (b *Backuper) openOrCreateGitRepo(root *os.Root) error {
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

	slog.Info("create new git repository in " + worktree)

	// not exists, create
	cmd := exec.Command("git", "init", worktree)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("can't initiate git repository in %q: %w", worktree, err)
	}

	return nil
}

func (b *Backuper) createGitBackup(root *os.Root, srcFilePath string) error {
	slog.Debug("backup file start (external)", "file", srcFilePath)

	err := b.openOrCreateGitRepo(root)
	if err != nil {
		return err
	}

	worktree := root.Name()

	cmd := exec.Command("git", "add", "--", srcFilePath)
	cmd.Dir = worktree

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("add file %q to git index error: %w", srcFilePath, err)
	}

	cmd = exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = worktree

	if err := cmd.Run(); err == nil {
		slog.Debug("backup skipped; no changes")

		return nil
	}

	msg := "backup file " + srcFilePath + " " + time.Now().Format(time.DateTime)

	cmd = exec.Command("git", "commit", "-m", msg)
	cmd.Dir = worktree

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("commit error: %w", err)
	}

	slog.Debug("backup committed")

	return nil
}
