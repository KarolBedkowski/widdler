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
	"strconv"
	"strings"
	"time"

	"github.com/sergi/go-diff/diffmatchpatch"
	_ "modernc.org/sqlite"
)

const (
	maxFileSize            = 1024 * 1024 * 128            // 128 MB
	createNextFullInterval = time.Duration(8) * time.Hour // create full backup every 8h
)

type BackuperSqlite struct {
	db       *sql.DB
	keepFull int
	keepIncr int
	compress bool
}

var _ BackupHandler = &BackuperSqlite{nil, 0, 0, false}

func newBackuperSqlite(ctx context.Context, dbfilename, policy string, compress bool) (BackuperSqlite, error) {
	conn, err := sql.Open("sqlite", dbfilename)
	if err != nil {
		return BackuperSqlite{}, fmt.Errorf("open database file failed: %w", err)
	}

	_, err = conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS backups (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT,
			filename TEXT, ts DATETIME, isfull INTEGER, compressed INTEGER, parent INTEGER,
			content BLOB);
		CREATE INDEX IF NOT EXISTS backups_idx ON backups (username, filename, isfull, ts);`)
	if err != nil {
		return BackuperSqlite{}, fmt.Errorf("init database failed: %w", err)
	}

	b := BackuperSqlite{
		db:       conn,
		keepFull: 0,
		keepIncr: 0,
		compress: compress,
	}
	if policy != "" {
		if err := b.loadPolicy(policy); err != nil {
			return BackuperSqlite{}, err
		}
	}

	return b, nil
}

func (b *BackuperSqlite) Create(ctx context.Context, root *os.Root, username, file string) error {
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

func (b *BackuperSqlite) Clean(ctx context.Context, users []string) error {
	if b.keepIncr == 0 && b.keepFull == 0 {
		return nil
	}

	for _, user := range users {
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin db transaction failed: %w", err)
		}

		if b.keepIncr > 0 {
			if err := b.deleteIncrBackups(ctx, tx, user); err != nil {
				_ = tx.Rollback()

				return err
			}
		}

		if b.keepFull > 0 {
			if err := b.deleteFullBackups(ctx, tx, user); err != nil {
				_ = tx.Rollback()

				return err
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit db transaction failed: %w", err)
		}
	}

	return nil
}

func (b *BackuperSqlite) deleteIncrBackups(ctx context.Context, tx *sql.Tx, user string) error {
	var backupid sql.NullInt64

	err := tx.QueryRowContext(ctx, "SELECT min(id) FROM ( "+
		"SELECT id FROM backups WHERE username=? AND isfull=0 ORDER BY ts desc LIMIT "+strconv.Itoa(b.keepIncr)+
		")", user).Scan(&backupid)
	if errors.Is(err, sql.ErrNoRows) {
		slog.DebugContext(ctx, "delete incr backups - not found", "user", user)

		return nil
	} else if err != nil {
		return fmt.Errorf("clean incr backup for user %q failed: %w", user, err)
	}

	slog.DebugContext(ctx, "delete incr backups; min id: "+strconv.Itoa(int(backupid.Int64)), "user", user)

	res, err := tx.ExecContext(ctx, "DELETE FROM backups WHERE username=? AND isfull=0 and id < ?",
		user, backupid)
	if err != nil {
		return fmt.Errorf("clean incr backup for user %q failed: %w", user, err)
	}

	count, _ := res.RowsAffected()
	slog.DebugContext(ctx, "delete incr backups; deleted: "+strconv.Itoa(int(count)), "user", user)

	return nil
}

func (b *BackuperSqlite) deleteFullBackups(ctx context.Context, tx *sql.Tx, user string) error {
	var backupid sql.NullInt64

	err := tx.QueryRowContext(ctx, "SELECT min(id) FROM ( "+
		"SELECT id FROM backups b WHERE username=? AND isfull=1 "+
		" AND NOT EXISTS (SELECT NULL FROM backups b2 WHERE b2.parent = b.id) "+
		"ORDER BY ts desc LIMIT "+strconv.Itoa(b.keepIncr)+
		")", user).Scan(&backupid)
	if errors.Is(err, sql.ErrNoRows) {
		slog.DebugContext(ctx, "delete full backups - not found", "user", user)

		return nil
	} else if err != nil {
		return fmt.Errorf("clean full backup for user %q failed: %w", user, err)
	}

	slog.DebugContext(ctx, "delete full backups; min id: "+strconv.Itoa(int(backupid.Int64)), "user", user)

	res, err := tx.ExecContext(ctx,
		"DELETE FROM backups as b WHERE username=? AND isfull=1 and id < ? "+
			" AND NOT EXISTS (SELECT NULL FROM backups b2 WHERE b2.parent = b.id) ",
		user, backupid)
	if err != nil {
		return fmt.Errorf("clean full backup for user %q failed: %w", user, err)
	}

	count, _ := res.RowsAffected()
	slog.DebugContext(ctx, "delete full backups; deleted: "+strconv.Itoa(int(count)), "user", user)

	return nil
}

func (b *BackuperSqlite) loadPolicy(policy string) error {
	fields := strings.Split(policy, ",")
	if len(fields) == 0 {
		return nil
	}

	var err error

	b.keepFull, err = strconv.Atoi(fields[0])
	if err != nil {
		return fmt.Errorf("invalid policy value %q: %w", fields[0], err)
	}

	if len(fields) > 1 {
		b.keepIncr, err = strconv.Atoi(fields[1])
		if err != nil {
			return fmt.Errorf("invalid policy value %q: %w", fields[1], err)
		}
	}

	return nil
}

func (b *BackuperSqlite) getPrevContent(ctx context.Context, username, file string) (string, int64, error) {
	var (
		content    []byte
		compressed int
		backupid   int64
	)

	maxFullTs := time.Now().Add(-createNextFullInterval)
	err := b.db.QueryRowContext(ctx, `
			SELECT id, compressed, content FROM backups
			WHERE username=? AND filename=? AND isfull=1 AND ts > ?
			ORDER BY ts DESC LIMIT 1`,
		username, file, maxFullTs.Unix()).
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

func (b *BackuperSqlite) storeContent(
	ctx context.Context,
	username, file string,
	isfull bool,
	content []byte,
	parentID int64,
) error {
	compressed := 0
	storefull := 0
	parentBackup := sql.NullInt64{Int64: parentID, Valid: true}

	if isfull {
		storefull = 1
		parentBackup.Valid = false
		parentBackup.Int64 = 0
	}

	// compressed only full content and bigger incrementals
	if b.compress && (isfull || len(content) > 1024) {
		var err error

		content, err = b.compressContent(content)
		if err != nil {
			return fmt.Errorf("compress content failed: %w", err)
		}

		compressed = 1
	}

	slog.DebugContext(ctx, "insert backup", "user", username, "file", file, "isfull", isfull,
		"compressed", compressed, "parent", parentID, "len", len(content))

	_, err := b.db.ExecContext(ctx, `
			INSERT INTO backups (username, filename, ts, isfull, content, compressed, parent)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
		username, file, time.Now().Unix(), storefull, content, compressed, parentBackup)
	if err != nil {
		return fmt.Errorf("insert new content failed: %w", err)
	}

	return nil
}

func (*BackuperSqlite) compressContent(content []byte) ([]byte, error) {
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
	if err != nil && !errors.Is(err, io.EOF) {
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

func RestoreSqliteBackup( //nolint:cyclop,funlen
	ctx context.Context,
	dbfilename string,
	backupid int64,
) ([]byte, error) {
	conn, err := sql.Open("sqlite", dbfilename)
	if err != nil {
		return nil, fmt.Errorf("open database file failed: %w", err)
	}

	defer conn.Close()

	var (
		content, parentContent []byte
		compressed, isfull, ts int
		parentCompressed       sql.NullInt32
		parentID               sql.NullInt64
	)

	err = conn.QueryRowContext(ctx, `
			SELECT b.compressed, b.content, b.isfull, b.ts, pb.compressed, pb.content, b.parent
			FROM backups b LEFT JOIN backups pb ON b.parent = pb.id
			WHERE b.id=?`,
		backupid).
		Scan(&compressed, &content, &isfull, &ts, &parentCompressed, &parentContent)
	if errors.Is(err, sql.ErrNoRows) || len(content) == 0 {
		return nil, errors.New("backup not found") //nolint:err113
	} else if err != nil {
		return nil, fmt.Errorf("get last full content failed: %w", err)
	}

	if compressed == 1 {
		content, err = decompressContent(content)
		if err != nil {
			return nil, fmt.Errorf("decompress content failed: %w", err)
		}
	}

	if isfull == 1 {
		return content, nil
	}

	if len(parentContent) == 0 || !parentID.Valid {
		return nil, fmt.Errorf("parent full backup (%d) not found", parentID.Int64) //nolint:err113
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
