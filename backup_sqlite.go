package main

//
// backup_sqlite.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//
import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/sergi/go-diff/diffmatchpatch"
	_ "modernc.org/sqlite"
)

type BackuperSqlite struct {
	db *sql.DB
}

func newBackuperSqlite(dbfilename string) (BackuperSqlite, error) {
	conn, err := sql.Open("sqlite", dbfilename)
	if err != nil {
		return BackuperSqlite{}, fmt.Errorf("open database file failed: %w", err)
	}

	ctx := context.Background()

	_, err = conn.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS backups "+
			" (username TEXT, filename TEXT, ts DATETIME, isfull INT, compressed INT, content BLOB);"+
			"CREATE INDEX IF NOT EXISTS backups_idx ON backups (username, filename, isfull, ts)")
	if err != nil {
		return BackuperSqlite{}, fmt.Errorf("init database failed: %w", err)
	}

	return BackuperSqlite{conn}, nil
}

func (b BackuperSqlite) createBackup(ctx context.Context, root *os.Root, username, file string) error {
	lastFullContent, err := b.getPrevContent(ctx, username, file)
	if err != nil {
		return fmt.Errorf("read prev backup failed: %w", err)
	}

	slog.DebugContext(ctx, "prev content", "len", len(lastFullContent))

	newContentB, err := root.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read file %q failed: %w", file, err)
	}

	isfull := len(lastFullContent) == 0
	if !isfull {
		dmp := diffmatchpatch.New()
		diffs := dmp.DiffMain(lastFullContent, string(newContentB), true)
		patch := dmp.PatchMake(diffs)
		newContentB = []byte(dmp.PatchToText(patch))
	}

	if len(newContentB) == 0 {
		slog.DebugContext(ctx, "backup skipped - no changes")

		return nil
	}

	if err := b.storeContent(ctx, username, file, isfull, newContentB); err != nil {
		return fmt.Errorf("store new backup failed: %w", err)
	}

	slog.DebugContext(ctx, "backup created", "full", isfull, "len", len(newContentB))

	return nil
}

func (b BackuperSqlite) getPrevContent(ctx context.Context, username, file string) (string, error) {
	var (
		lastFullContentB []byte
		compressed       int
	)

	now := time.Now().Truncate(24 * time.Hour).Unix() //nolint:mnd

	err := b.db.QueryRowContext(ctx,
		"SELECT compressed, content FROM backups "+
			"WHERE username=? AND filename=? AND isfull=1 AND ts > ? "+
			"ORDER BY ts DESC LIMIT 1",
		username, file, now).
		Scan(&compressed, &lastFullContentB)
	if errors.Is(err, sql.ErrNoRows) || len(lastFullContentB) == 0 {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("get last full content failed: %w", err)
	}

	if compressed == 1 {
		buf := bytes.NewBuffer(lastFullContentB)
		zr, _ := gzip.NewReader(buf)

		var bufout bytes.Buffer

		_, err := io.Copy(&bufout, zr)
		if err != nil {
			return "", fmt.Errorf("compress content failed: %w", err)
		}

		zr.Close()

		return bufout.String(), nil
	}

	return string(lastFullContentB), nil
}

func (b BackuperSqlite) storeContent(ctx context.Context, username, file string, isfull bool, content []byte) error {
	compressed := 0
	storefull := 0

	if isfull {
		// compressed only full content
		var err error

		content, err = b.compressContent(content)
		if err != nil {
			return fmt.Errorf("compress content failed: %w", err)
		}

		compressed = 1
		storefull = 1
	}

	_, err := b.db.ExecContext(ctx,
		"INSERT INTO backups (username, filename, ts, isfull, content, compressed) "+
			"VALUES (?, ?, ?, ?, ?, ?)",
		username, file, time.Now().Unix(), storefull, content, compressed)
	if err != nil {
		return fmt.Errorf("insert new content failed: %w", err)
	}

	return nil
}

func (BackuperSqlite) compressContent(content []byte) ([]byte, error) {
	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)

	_, err := zw.Write(content)
	if err != nil {
		return nil, fmt.Errorf("compress content error: %w", err)
	}

	zw.Close()

	return buf.Bytes(), nil
}
