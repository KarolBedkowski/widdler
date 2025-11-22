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

const maxFileSize = 1024 * 1024 * 128 // 128 MB

type BackuperSqlite struct {
	db *sql.DB
}

var _ BackupHandler = &BackuperSqlite{nil}

func newBackuperSqlite(ctx context.Context, dbfilename string) (BackuperSqlite, error) {
	conn, err := sql.Open("sqlite", dbfilename)
	if err != nil {
		return BackuperSqlite{}, fmt.Errorf("open database file failed: %w", err)
	}

	_, err = conn.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS backups "+
			" (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, filename TEXT, ts DATETIME, "+
			"isfull INTEGER, compressed INTEGER, parent INTEGER, content BLOB);"+
			"CREATE INDEX IF NOT EXISTS backups_idx ON backups (username, filename, isfull, ts)")
	if err != nil {
		return BackuperSqlite{}, fmt.Errorf("init database failed: %w", err)
	}

	return BackuperSqlite{conn}, nil
}

func (b BackuperSqlite) Create(ctx context.Context, root *os.Root, username, file string) error {
	lastFullContent, parentID, err := b.getPrevContent(ctx, username, file)
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

	if err := b.storeContent(ctx, username, file, isfull, newContentB, parentID); err != nil {
		return fmt.Errorf("store new backup failed: %w", err)
	}

	slog.DebugContext(ctx, "backup created", "full", isfull, "len", len(newContentB))

	return nil
}

func (b BackuperSqlite) Clean(ctx context.Context, users []string) error { //nolint:revive
	return nil
}

func (b BackuperSqlite) getPrevContent(ctx context.Context, username, file string) (string, int64, error) {
	var (
		content    []byte
		compressed int
		backupid   int64
	)

	now := time.Now().Truncate(24 * time.Hour).Unix() //nolint:mnd

	err := b.db.QueryRowContext(ctx,
		"SELECT id, compressed, content FROM backups "+
			"WHERE username=? AND filename=? AND isfull=1 AND ts > ? "+
			"ORDER BY ts DESC LIMIT 1",
		username, file, now).
		Scan(&backupid, &compressed, &content)
	if errors.Is(err, sql.ErrNoRows) || len(content) == 0 {
		return "", 0, nil
	} else if err != nil {
		return "", 0, fmt.Errorf("get last full content failed: %w", err)
	}

	if compressed == 1 {
		con, err := decompressContent(content)
		if err != nil {
			return "", 0, fmt.Errorf("decompress content failed: %w", err)
		}

		return string(con), backupid, nil
	}

	return string(content), backupid, nil
}

func (b BackuperSqlite) storeContent(
	ctx context.Context,
	username, file string,
	isfull bool,
	content []byte,
	parentID int64,
) error {
	compressed := 0
	storefull := 0

	dbparentid := sql.NullInt64{Int64: parentID, Valid: true}

	if isfull {
		// compressed only full content
		var err error

		content, err = b.compressContent(content)
		if err != nil {
			return fmt.Errorf("compress content failed: %w", err)
		}

		compressed = 1
		storefull = 1
		dbparentid.Valid = false
		dbparentid.Int64 = 0
	}

	_, err := b.db.ExecContext(ctx,
		"INSERT INTO backups (username, filename, ts, isfull, content, compressed, parent) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		username, file, time.Now().Unix(), storefull, content, compressed, dbparentid)
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

func decompressContent(content []byte) ([]byte, error) {
	buf := bytes.NewBuffer(content)
	zr, _ := gzip.NewReader(buf)

	var bufout bytes.Buffer

	_, err := io.CopyN(&bufout, zr, maxFileSize)
	if err != nil {
		return nil, fmt.Errorf("compress content failed: %w", err)
	}

	zr.Close()

	return bufout.Bytes(), nil
}

type SqliteBackup struct {
	ID        int64
	Username  string
	Timestamp time.Time
	Filename  string
	IsFull    bool
}

func (s SqliteBackup) ToString() string {
	return fmt.Sprintf("%d: %s %s %s (full=%v)", s.ID, s.Username, s.Timestamp, s.Filename, s.IsFull)
}

func ListSqliteBackups(ctx context.Context, dbfilename, username string) ([]SqliteBackup, error) {
	conn, err := sql.Open("sqlite", dbfilename)
	if err != nil {
		return nil, fmt.Errorf("open database file failed: %w", err)
	}

	defer conn.Close()

	var rows *sql.Rows

	if username != "" {
		rows, err = conn.QueryContext(ctx,
			"SELECT id, username, ts, filename, isfull FROM backups WHERE username=? ORDER BY ts",
			username)
	} else {
		rows, err = conn.QueryContext(ctx,
			"SELECT id, username, ts, filename, isfull FROM backups ORDER BY ts")
	}

	if err != nil {
		return nil, fmt.Errorf("query backups failed: %w", err)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query backups failed: %w", err)
	}

	defer rows.Close()

	backups := make([]SqliteBackup, 0)

	for rows.Next() {
		sb := SqliteBackup{} //nolint:exhaustruct

		var ts, isfull int64

		if err := rows.Scan(&sb.ID, &sb.Username, &ts, &sb.Filename, &isfull); err != nil {
			return nil, fmt.Errorf("scan error: %w", err)
		}

		sb.IsFull = isfull > 0
		sb.Timestamp = time.Unix(ts, 0)

		backups = append(backups, sb)
	}

	return backups, nil
}

func RestoreSqliteBackup(ctx context.Context, dbfilename string, backupid int64) ([]byte, error) { //nolint:cyclop
	conn, err := sql.Open("sqlite", dbfilename)
	if err != nil {
		return nil, fmt.Errorf("open database file failed: %w", err)
	}

	defer conn.Close()

	var (
		content, parentContent []byte
		compressed, isfull, ts int
		parentCompressed       sql.NullInt32
	)

	err = conn.QueryRowContext(ctx,
		"SELECT b.compressed, b.content, b.isfull, b.ts, pb.compressed, pb.content "+
			"FROM backups b LEFT JOIN backups pb ON b.parent = pb.id "+
			"WHERE b.id=?",
		backupid).
		Scan(&compressed, &content, &isfull, &ts, &parentCompressed, &parentContent)
	if errors.Is(err, sql.ErrNoRows) || len(content) == 0 {
		return nil, errors.New("backup not found") //nolint:err113
	} else if err != nil {
		return nil, fmt.Errorf("get last full content failed: %w", err)
	}

	if compressed == 1 {
		return decompressContent(content)
	}

	if parentCompressed.Valid && parentCompressed.Int32 == 1 {
		parentContent, err = decompressContent(parentContent)
		if err != nil {
			return nil, fmt.Errorf("decompress parent backup failed: %w", err)
		}
	}

	dmp := diffmatchpatch.New()

	patches, err := dmp.PatchFromText(string(content))
	if err != nil {
		return nil, fmt.Errorf("failed to load patch: %w", err)
	}

	result, oks := dmp.PatchApply(patches, string(parentContent))
	for i, ok := range oks {
		if !ok {
			return nil, fmt.Errorf("failed to apply patch %d (%q): %w", i, patches[i], err)
		}
	}

	return []byte(result), nil
}
