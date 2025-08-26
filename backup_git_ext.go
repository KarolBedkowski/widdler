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
	case err != nil:
		return fmt.Errorf("get stat of .git directory in %q error: %w", root.Name(), err)
	case !st.IsDir():
		return ErrInvalidGitDir
	default:
		return nil
	}

	// not exists, create

	cmd := exec.Command("git", "init", root.Name()) //nolint:gosec
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("can't initiate git repository in %q: %w", root.Name(), err)
	}

	return nil
}

func (b *Backuper) createGitBackup(root *os.Root, srcFilePath string) error {
	err := b.openOrCreateGitRepo(root)
	if err != nil {
		return err
	}

	worktree := root.Name()

	cmd := exec.Command("git", "add", "--", srcFilePath)
	cmd.Dir = worktree

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("add file to git index error: %w", err)
	}

	cmd = exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = worktree

	if err := cmd.Run(); err == nil {
		slog.Debug("file not modified")

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
