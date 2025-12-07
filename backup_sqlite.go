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
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
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

//go:embed tmpl/*.tmpl
var templatesFS embed.FS
var backupListTmpl *template.Template

func init() {
	backupListTmpl = template.Must(template.ParseFS(templatesFS, "tmpl/*.tmpl"))
}

//------------------------------------------------------------------------------

type BackuperSqlite struct {
	db               *sql.DB
	numFullBackups   int
	numIncrBackups   int
	maxFullBackupAge time.Duration
	compress         bool
}

var _ BackupHandler = &BackuperSqlite{} //nolint:exhaustruct

func newBackuperSqlite(ctx context.Context, dbfilename, policy string, compress bool) (BackuperSqlite, error) {
	db, err := sql.Open("sqlite", dbfilename)
	if err != nil {
		return BackuperSqlite{}, fmt.Errorf("open database file failed: %w", err)
	}

	db.SetConnMaxLifetime(60) //nolint:mnd
	db.SetMaxIdleConns(1)
	db.SetMaxOpenConns(5) //nolint:mnd

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS backups (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT,
			filename TEXT, ts DATETIME, isfull INTEGER, compressed INTEGER, parent INTEGER,
			content BLOB);
		CREATE INDEX IF NOT EXISTS backups_idx ON backups (username, filename, isfull, ts);`)
	if err != nil {
		return BackuperSqlite{}, fmt.Errorf("init database failed: %w", err)
	}

	backuper := BackuperSqlite{
		db:               db,
		numFullBackups:   7, //nolint:mnd
		numIncrBackups:   7, //nolint:mnd
		maxFullBackupAge: createNextFullInterval,
		compress:         compress,
	}
	if policy != "" {
		if err := backuper.loadPolicy(policy); err != nil {
			return BackuperSqlite{}, err
		}
	}

	return backuper, nil
}

func (b *BackuperSqlite) Create(ctx context.Context, root *os.Root, username, file string) error {
	conn, err := b.getConnection(ctx)
	if err != nil {
		return err
	}

	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin db transaction failed: %w", err)
	}

	defer tx.Rollback() //nolint:errcheck

	var (
		lastFullContent string
		parentID        int64
	)

	if b.numIncrBackups > 0 {
		lastFullContent, parentID, err = b.getPrevContent(ctx, tx, username, file)
		if err != nil {
			return fmt.Errorf("read prev backup failed: %w", err)
		}

		slog.DebugContext(ctx, "prev content", "len", len(lastFullContent))
	}

	newContentB, err := root.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read file %q failed: %w", file, err)
	}

	isfull := len(lastFullContent) == 0
	if !isfull {
		dmp := diffmatchpatch.New()
		diffs := dmp.DiffMain(lastFullContent, string(newContentB), false)
		patch := dmp.PatchMake(diffs)
		newContentB = []byte(dmp.PatchToText(patch))
	}

	if len(newContentB) == 0 {
		slog.DebugContext(ctx, "backup skipped - no changes")

		return nil
	}

	if err := b.storeContent(ctx, tx, username, file, isfull, newContentB, parentID); err != nil {
		return fmt.Errorf("store new backup failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit changes to db failed: %w", err)
	}

	slog.DebugContext(ctx, "backup created", "full", isfull, "len", len(newContentB))

	return nil
}

func (b *BackuperSqlite) Clean(ctx context.Context, users []string) error { //nolint:cyclop,revive
	if b.numIncrBackups == 0 && b.numFullBackups == 0 {
		return nil
	}

	conn, err := b.getConnection(ctx)
	if err != nil {
		return fmt.Errorf("failed to open db connection: %w", err)
	}

	defer conn.Close()

	usersfiles, err := b.getUsersFiles(ctx)
	if err != nil {
		return fmt.Errorf("failed to get users files: %w", err)
	}

	for _, userfile := range usersfiles {
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin db transaction failed: %w", err)
		}

		if b.numIncrBackups > 0 {
			if err := b.deleteIncrBackups(ctx, tx, userfile.Username, userfile.Filename); err != nil {
				_ = tx.Rollback()

				return err
			}
		}

		if b.numFullBackups > 0 {
			if err := b.deleteFullBackups(ctx, tx, userfile.Username, userfile.Filename); err != nil {
				_ = tx.Rollback()

				return err
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit db transaction failed: %w", err)
		}
	}

	if _, err := b.db.ExecContext(ctx, "VACUUM; PRAGMA optimize;"); err != nil {
		return fmt.Errorf("run vacuum failed: %w", err)
	}

	return nil
}

func (b *BackuperSqlite) ListHandler(ctx context.Context, w http.ResponseWriter, r *http.Request,
	root *os.Root, user string,
) bool {
	url := r.URL.Path

	slog.DebugContext(ctx, "ListHandler", "url", url)

	prefix := "/_backups"
	if user != "" {
		prefix = "/" + user + prefix
	}

	if url == prefix || url == prefix+"/" {
		return b.listBackupsHandler(ctx, w, user, prefix)
	}

	prefix += "/"

	if !strings.HasPrefix(url, prefix) {
		return false
	}

	parts := strings.Split(strings.TrimPrefix(url, prefix), "/")
	if len(parts) != 2 { //nolint:mnd
		return false
	}

	backupid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		slog.ErrorContext(ctx, "parse backup id failed", "err", err, "parts", parts)
		w.WriteHeader(http.StatusBadRequest)

		return false
	}

	switch parts[1] {
	case "view":
		return b.viewBackupHandler(ctx, w, user, backupid)
	case "restore":
		return b.restoreBackupHandler(ctx, w, r, root, user, backupid)
	}

	return false
}

func (b *BackuperSqlite) listBackupsHandler(ctx context.Context, w http.ResponseWriter,
	user, prefix string,
) bool {
	conn, err := b.getConnection(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "ListHandler failed to get db connection", "err", err)
		w.WriteHeader(http.StatusInternalServerError)

		return false
	}

	backups, err := listSqliteBackups(ctx, conn, user)
	if err != nil {
		slog.ErrorContext(ctx, "ListHandler failed to get backups", "err", err)
		w.WriteHeader(http.StatusInternalServerError)

		return false
	}

	data := struct {
		Backups []SqliteBackup
		Prefix  string
	}{
		backups,
		prefix,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := backupListTmpl.ExecuteTemplate(w, "sqliteindexpage.tmpl", &data); err != nil {
		slog.ErrorContext(ctx, "ListHandler failed to render", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
	}

	return true
}

func (b *BackuperSqlite) viewBackupHandler(ctx context.Context, w http.ResponseWriter, user string, backupid int64,
) bool {
	conn, err := b.getConnection(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "get db connection failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)

		return true
	}

	defer conn.Close()

	data, err := getFileFromDb(ctx, conn, backupid)
	if err != nil {
		slog.ErrorContext(ctx, "get file form db connection failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)

		return true
	}

	if data.Username != user {
		slog.ErrorContext(ctx, "invalid user", "backup.user", data.Username, "user", user)
		w.WriteHeader(http.StatusNotFound)

		return true
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data.Data)

	return false
}

func (b *BackuperSqlite) restoreBackupHandler(ctx context.Context, w http.ResponseWriter, r *http.Request,
	root *os.Root, user string, backupid int64,
) bool {
	conn, err := b.getConnection(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "get db connection failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)

		return true
	}

	defer conn.Close()

	data, err := getFileFromDb(ctx, conn, backupid)
	if err != nil {
		slog.ErrorContext(ctx, "get file form db connection failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)

		return true
	}

	if data.Username != user {
		slog.ErrorContext(ctx, "invalid user", "backup.user", data.Username, "user", user)
		w.WriteHeader(http.StatusNotFound)

		return true
	}

	fname := r.FormValue("filename")

	if r.Method == http.MethodPost && fname != "" {
		if err := root.WriteFile(fname, data.Data, 0o660); err != nil { //nolint:mnd
			slog.ErrorContext(ctx, "write file error", "backup.filename", data.Filename, "err", err)
			w.WriteHeader(http.StatusInternalServerError)

			return true
		}

		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusFound)

		return true
	}

	if err := backupListTmpl.ExecuteTemplate(w, "sqliterestorepage.tmpl", &data); err != nil {
		slog.ErrorContext(ctx, "restoreBackupHandler failed to render", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
	}

	return true
}

func (b *BackuperSqlite) getConnection(ctx context.Context) (*sql.Conn, error) {
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open db connection failed: %w", err)
	}

	_, err = conn.ExecContext(ctx, `
PRAGMA page_size = 8192;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA temp_store = MEMORY;
`)
	if err != nil {
		conn.Close()

		return nil, fmt.Errorf("run db init conn script failed: %w", err)
	}

	return conn, nil
}

type UserFile struct {
	Username string
	Filename string
}

func (b *BackuperSqlite) getUsersFiles(ctx context.Context) ([]UserFile, error) {
	var usersfiles []UserFile

	rows, err := b.db.QueryContext(ctx, "SELECT DISTINCT username, filename FROM BACKUPS")
	if err != nil {
		return nil, fmt.Errorf("get users files failed: %w", err)
	}

	defer rows.Close()

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get users files failed: %w", err)
	}

	for rows.Next() {
		uf := UserFile{} //nolint:exhaustruct

		if err := rows.Scan(&uf.Username, &uf.Filename); err != nil {
			return nil, fmt.Errorf("scan user files error: %w", err)
		}

		usersfiles = append(usersfiles, uf)
	}

	return usersfiles, nil
}

func (b *BackuperSqlite) deleteIncrBackups(ctx context.Context, tx *sql.Tx, user, file string) error {
	var backupid sql.NullInt64

	err := tx.QueryRowContext(ctx, "SELECT min(id) FROM ( "+
		"SELECT id FROM backups WHERE username=? AND filename=? AND isfull=0 "+
		"ORDER BY ts DESC LIMIT "+strconv.Itoa(b.numIncrBackups)+
		")", user, file).Scan(&backupid)
	if errors.Is(err, sql.ErrNoRows) {
		slog.DebugContext(ctx, "delete incr backups - not found", "user", user, "file", file)

		return nil
	} else if err != nil {
		return fmt.Errorf("clean incr backup for user %q file %q failed: %w", user, file, err)
	}

	slog.DebugContext(ctx, "delete incr backups", "min_id", backupid.Int64, "user", user, "file", file)

	res, err := tx.ExecContext(ctx, "DELETE FROM backups WHERE username=? AND filename=? AND isfull=0 and id<?",
		user, file, backupid)
	if err != nil {
		return fmt.Errorf("clean incr backup for user %q file %q failed: %w", user, file, err)
	}

	count, _ := res.RowsAffected()
	slog.DebugContext(ctx, "delete incr backups", "deleted", count, "user", user, "file", file)

	return nil
}

func (b *BackuperSqlite) deleteFullBackups(ctx context.Context, tx *sql.Tx, user, file string) error {
	var backupid sql.NullInt64

	err := tx.QueryRowContext(ctx, "SELECT min(id) FROM ( "+
		"SELECT id FROM backups b WHERE username=? AND filename=? AND isfull=1 "+
		" AND NOT EXISTS (SELECT NULL FROM backups b2 WHERE b2.parent = b.id) "+
		"ORDER BY ts DESC LIMIT "+strconv.Itoa(b.numIncrBackups)+
		")", user, file).Scan(&backupid)
	if errors.Is(err, sql.ErrNoRows) {
		slog.DebugContext(ctx, "delete full backups - not found", "user", user, "file", file)

		return nil
	} else if err != nil {
		return fmt.Errorf("clean full backup for user %q file %q failed: %w", user, file, err)
	}

	slog.DebugContext(ctx, "delete full backups", "min_id", backupid.Int64, "user", user, "file", file)

	res, err := tx.ExecContext(ctx,
		"DELETE FROM backups as b WHERE username=? AND filename=? AND isfull=1 and id < ? "+
			" AND NOT EXISTS (SELECT NULL FROM backups b2 WHERE b2.parent = b.id) ",
		user, file, backupid)
	if err != nil {
		return fmt.Errorf("clean full backup for user %q file %q failed: %w", user, file, err)
	}

	count, _ := res.RowsAffected()
	slog.DebugContext(ctx, "delete full backups", "deleted", count, "user", user, "file", file)

	return nil
}

// loadPolicy parse and set backup policy.
// Format: up to 3 fields separated by ',':
// 1. number of full backups to keep; must be > 0; default 7. Also keep backup which incremental backups depend on.
// 2. number of incremental backups to keep (optional). Missing or 0 disable incremental backups. Default 7.
// 3. time between create full backup (optional). Default createNextFullInterval.
func (b *BackuperSqlite) loadPolicy(policy string) error { //nolint:cyclop
	fields := strings.Split(policy, ",")
	if len(fields) == 0 {
		return nil
	}

	var err error

	if fields[0] != "" {
		b.numFullBackups, err = strconv.Atoi(fields[0])
		if err != nil {
			return fmt.Errorf("invalid policy value for number of full backups %q: %w", fields[0], err)
		}

		if b.numFullBackups <= 0 {
			return fmt.Errorf("invalid policy value for number of full backups %q - must be greater than 0", fields[0])
		}
	}

	if len(fields) > 1 && fields[1] != "" {
		b.numIncrBackups, err = strconv.Atoi(fields[1])
		if err != nil {
			return fmt.Errorf("invalid policy value for number of incremental backups %q: %w", fields[1], err)
		}

		if b.numIncrBackups < 0 {
			return fmt.Errorf("invalid policy value for number of incremental backups %q - must be greater or equal 0",
				fields[1])
		}
	}

	if len(fields) > 2 && fields[2] != "" {
		b.maxFullBackupAge, err = time.ParseDuration(fields[2])
		if err != nil {
			return fmt.Errorf("invalid policy value for full backup interval %q: %w", fields[2], err)
		}

		if b.maxFullBackupAge < 0 {
			return fmt.Errorf( //nolint:err113
				"invalid policy value for full backup interval %q; must be greater than 0", fields[2])
		}
	}

	slog.Info(fmt.Sprintf("backup policy: %d full backups, %d incremental backups, %s between full backups",
		b.numFullBackups, b.numIncrBackups, b.maxFullBackupAge))

	return nil
}

func (b *BackuperSqlite) getPrevContent(ctx context.Context, tx *sql.Tx, username, file string) (string, int64, error) {
	var (
		content    []byte
		compressed int
		backupid   int64
	)

	maxFullTs := time.Now().Add(-b.maxFullBackupAge)
	err := tx.QueryRowContext(ctx, `
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
	tx *sql.Tx,
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

	_, err := tx.ExecContext(ctx, `
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
	zw.Close()

	if err != nil {
		return nil, fmt.Errorf("compress content error: %w", err)
	}

	return buf.Bytes(), nil
}

//------------------------------------------------------------------------------

func decompressContent(content []byte) ([]byte, error) {
	buf := bytes.NewBuffer(content)
	zr, _ := gzip.NewReader(buf)

	var bufout bytes.Buffer

	_, err := io.CopyN(&bufout, zr, maxFileSize)
	zr.Close()

	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("compress content failed: %w", err)
	}

	return bufout.Bytes(), nil
}

//------------------------------------------------------------------------------

type SqliteBackup struct {
	Timestamp time.Time
	Username  string
	Filename  string
	ID        int64
	IsFull    bool
	Data      []byte
}

func (s SqliteBackup) ToString() string {
	kind := "incr"
	if s.IsFull {
		kind = "FULL"
	}

	return fmt.Sprintf("%4d | %-10s | %-30s | %-20s | %s", s.ID, s.Username, s.Timestamp, s.Filename, kind)
}

//------------------------------------------------------------------------------

func ListSqliteBackups(ctx context.Context, dbfilename, username string) ([]SqliteBackup, error) {
	db, err := sql.Open("sqlite", dbfilename)
	if err != nil {
		return nil, fmt.Errorf("open database file failed: %w", err)
	}

	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open db connection failed: %w", err)
	}

	defer conn.Close()

	return listSqliteBackups(ctx, conn, username)
}

func listSqliteBackups(ctx context.Context, conn *sql.Conn, username string) ([]SqliteBackup, error) {
	var (
		rows *sql.Rows
		err  error
	)

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

// ------------------------------------------------------------------------------
func RestoreSqliteBackup(
	ctx context.Context,
	dbfilename string,
	backupid int64,
) ([]byte, error) {
	db, err := sql.Open("sqlite", dbfilename)
	if err != nil {
		return nil, fmt.Errorf("open database file failed: %w", err)
	}

	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open db connection failed: %w", err)
	}

	defer conn.Close()

	backup, err := getFileFromDb(ctx, conn, backupid)
	if err != nil {
		return nil, fmt.Errorf("get file from db failed: %w", err)
	}

	return backup.Data, nil
}

//------------------------------------------------------------------------------

func getFileFromDb(ctx context.Context, conn *sql.Conn, backupid int64) (*SqliteBackup, error) { //nolint:cyclop
	var (
		content, parentContent        []byte
		compressed, isfull, timestamp int
		parentCompressed              sql.NullInt32
		parentID                      sql.NullInt64
	)

	backup := SqliteBackup{ID: backupid} //nolint:exhaustruct

	err := conn.QueryRowContext(ctx, `
			SELECT b.compressed, b.content, b.isfull, b.ts, pb.compressed, pb.content, b.parent, b.username,
				b.filename
			FROM backups b LEFT JOIN backups pb ON b.parent = pb.id
			WHERE b.id=?`,
		backupid).
		Scan(&compressed, &content, &isfull, &timestamp, &parentCompressed, &parentContent, &parentID,
			&backup.Username, &backup.Filename)
	if errors.Is(err, sql.ErrNoRows) {
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

	backup.Timestamp = time.Unix(int64(timestamp), 0)

	if isfull == 1 {
		backup.Data = content
		backup.IsFull = true

		return &backup, nil
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

	backup.Data = []byte(result)

	return &backup, nil
}
