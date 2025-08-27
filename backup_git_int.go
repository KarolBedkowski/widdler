//go:build gitint
// +build gitint

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
	"log/slog"
	"os"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func (b *Backuper) openOrCreateGitRepo(root *os.Root) (*git.Repository, error) {
	repo, err := git.PlainOpen(root.Name())
	if err == nil {
		return repo, nil
	}

	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("open git repository in %q failed: %w", root.Name(), err)
	}

	worktree := root.Name()

	slog.Info("create new git repository in " + worktree)

	repo, err = git.PlainInit(worktree, false)
	if err != nil {
		return nil, fmt.Errorf("init git repository in %q failed: %w", worktree, err)
	}

	return repo, nil
}

func (b *Backuper) createGitBackup(root *os.Root, srcFilePath string) error {
	slog.Debug("backup file start (internal)", "file", srcFilePath)

	r, err := b.openOrCreateGitRepo(root)
	if err != nil {
		return err
	}

	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("open worktree failed: %w", err)
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
