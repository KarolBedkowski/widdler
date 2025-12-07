//go:build gitint

package internal

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
	"log/slog"
	"os"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

type BackuperGit struct{}

var _ BackupHandler = &BackuperGit{}

func (b BackuperGit) Create(ctx context.Context, root *os.Root, username, srcFilePath string) error {
	slog.DebugContext(ctx, "backup file start (internal)", "file", srcFilePath, "username", username)

	r, err := b.openOrCreateGitRepo(ctx, root)
	if err != nil {
		return err
	}

	slog.DebugContext(ctx, "backup - open worktree")

	worktree, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("open worktree failed: %w", err)
	}

	slog.DebugContext(ctx, "backup - add file")

	if _, err = worktree.Add(srcFilePath); err != nil {
		return fmt.Errorf("add file %q to git repository in %q failed: %w", srcFilePath, root.Name(), err)
	}

	slog.DebugContext(ctx, "backup - commit")

	// It faster to commit and handle errors than check changed files before.
	// Esp that worktree.Status not always return useful data...

	user, ok := ctx.Value(CtxUserKey).(string)
	if !ok || user == "" {
		user = "widdler"
	}

	go func() {
		commit, err := worktree.Commit("backup file "+srcFilePath+" "+time.Now().Format(time.DateTime),
			&git.CommitOptions{ //nolint:exhaustruct
				Author: &object.Signature{
					Name:  user,
					Email: user + "@no.email",
					When:  time.Now(),
				},
			})

		if errors.Is(err, git.ErrEmptyCommit) {
			slog.DebugContext(ctx, "backup skipped; no changes")

			return
		}

		if err != nil {
			slog.ErrorContext(ctx, "commit to git repository failed", "repository", srcFilePath, "err", err)

			return
		}

		slog.DebugContext(ctx, "backup committed", "commit", commit.String())
	}()

	return nil
}

func (b BackuperGit) Clean(ctx context.Context, users []string) error { //nolint:revive
	return nil
}

func (b BackuperGit) openOrCreateGitRepo(ctx context.Context, root *os.Root) (*git.Repository, error) {
	repo, err := git.PlainOpen(root.Name())
	if err == nil {
		return repo, nil
	}

	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("open git repository in %q failed: %w", root.Name(), err)
	}

	worktree := root.Name()

	slog.InfoContext(ctx, "create new git repository in "+worktree)

	repo, err = git.PlainInit(worktree, false)
	if err != nil {
		return nil, fmt.Errorf("init git repository in %q failed: %w", worktree, err)
	}

	return repo, nil
}
