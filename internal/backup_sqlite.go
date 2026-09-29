package internal

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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gitlab.com/tozd/go/errors"

	"github.com/sergi/go-diff/diffmatchpatch"
	slogctx "github.com/veqryn/slog-context"
)

const (
	maxFileSize            = 1024 * 1024 * 128            // 128 MB
	createNextFullInterval = time.Duration(8) * time.Hour // create full backup every 8h
)

//------------------------------------------------------------------------------

type BackuperSqlite struct {
	db               *sql.DB
	numFullBackups   int
	numIncrBackups   int
	maxFullBackupAge time.Duration
	compress         bool
}

var _ BackupHandler = &BackuperSqlite{} //nolint:exhaustruct

func newBackuperSqlite(ctx context.Context, dbfilename, policy string, compress bool) (*BackuperSqlite, error) {
	db, err := sql.Open("sqlite", dbfilename)
	if err != nil {
		return nil, errors.Wrap(err, "open database file")
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
		return nil, errors.Wrap(err, "execute initial sql")
	}

	backuper := &BackuperSqlite{
		db:               db,
		numFullBackups:   7, //nolint:mnd
		numIncrBackups:   7, //nolint:mnd
		maxFullBackupAge: createNextFullInterval,
		compress:         compress,
	}
	if policy != "" {
		if err := backuper.loadPolicy(policy); err != nil {
			return nil, err
		}
	}

	return backuper, nil
}

func (b *BackuperSqlite) Create(ctx context.Context, root *os.Root, username, file string) error {
	conn, err := b.getConnection(ctx)
	if err != nil {
		return errors.WithMessage(err, "get connection")
	}

	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "begin tx")
	}

	defer tx.Rollback() //nolint:errcheck

	var (
		lastFullContent string
		parentID        int64
	)

	if b.numIncrBackups > 0 {
		lastFullContent, parentID, err = b.getPrevContent(ctx, tx, username, file)
		if err != nil {
			return errors.WithMessage(err, "read prev backup from db")
		}

		slog.DebugContext(ctx, "sqlitebackup: loaded prev content", "backup_size", len(lastFullContent))
	}

	newContentB, err := root.ReadFile(file)
	if err != nil {
		return errors.WithMessagef(err, "read file %q", file)
	}

	isfull := len(lastFullContent) == 0
	if !isfull {
		dmp := diffmatchpatch.New()
		diffs := dmp.DiffMain(lastFullContent, string(newContentB), false)
		patch := dmp.PatchMake(diffs)
		newContentB = []byte(dmp.PatchToText(patch))
	}

	if len(newContentB) == 0 {
		slog.DebugContext(ctx, "sqlitebackup: backup skipped - no changes")

		return nil
	}

	if err := b.storeContent(ctx, tx, username, file, isfull, newContentB, parentID); err != nil {
		return errors.WithMessage(err, "store new backup")
	}

	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "commit changes")
	}

	slog.DebugContext(ctx, "sqlitebackup: backup created ", "full_backup", isfull, "backup_size", len(newContentB))

	return nil
}

func (b *BackuperSqlite) Clean(ctx context.Context, users []string) error { //nolint:cyclop,revive
	if b.numIncrBackups == 0 && b.numFullBackups == 0 {
		return nil
	}

	conn, err := b.getConnection(ctx)
	if err != nil {
		return errors.WithMessage(err, "get db connection")
	}

	defer conn.Close()

	usersfiles, err := b.getUsersFiles(ctx)
	if err != nil {
		return errors.WithMessage(err, "get users files")
	}

	for _, userfile := range usersfiles {
		lctx := slogctx.Append(ctx, slog.String("user", userfile.Username), slog.String("file", userfile.Filename))

		tx, err := b.db.BeginTx(lctx, nil)
		if err != nil {
			return errors.Wrap(err, "begin tx")
		}

		if b.numIncrBackups > 0 {
			if err := b.deleteIncrBackups(lctx, tx, userfile.Username, userfile.Filename); err != nil {
				_ = tx.Rollback()

				return err
			}
		}

		if b.numFullBackups > 0 {
			if err := b.deleteFullBackups(lctx, tx, userfile.Username, userfile.Filename); err != nil {
				_ = tx.Rollback()

				return err
			}
		}

		if err := tx.Commit(); err != nil {
			return errors.Wrap(err, "commit")
		}
	}

	if _, err := b.db.ExecContext(ctx, "VACUUM; PRAGMA optimize;"); err != nil {
		return errors.Wrap(err, "run vacuum")
	}

	return nil
}

func (b *BackuperSqlite) ListHandler(ctx context.Context, w http.ResponseWriter, r *http.Request,
	root *os.Root, user string,
) bool {
	url := r.URL.Path
	prefix := "/_backups"

	if !strings.HasPrefix(url, prefix) {
		return false
	}

	slog.DebugContext(ctx, "sqlitebackup: list backups", "url", url)

	if url == prefix || url == prefix+"/" {
		b.listBackupsHandler(ctx, w, user, prefix)

		return true
	}

	prefix += "/"

	// url in form '<prefix>/<backupid>/<action>'
	parts := strings.Split(strings.TrimPrefix(url, prefix), "/")
	if len(parts) != 2 { //nolint:mnd
		return false
	}

	backupid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		slog.ErrorContext(ctx, "sqlitebackup: parse backup id failed", "err", err, "parts", parts)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)

		return false
	}

	switch parts[1] {
	case "view":
		b.viewBackupHandler(ctx, w, user, backupid)
	case "restore":
		b.restoreBackupHandler(ctx, w, r, root, user, backupid)
	default:
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	}

	return true
}

func (b *BackuperSqlite) listBackupsHandler(ctx context.Context, w http.ResponseWriter, user, prefix string) {
	conn, err := b.getConnection(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "sqlitebackup: failed to get db connection", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	backups, err := listSqliteBackups(ctx, conn, user)
	if err != nil {
		slog.ErrorContext(ctx, "sqlitebackup: failed to get backups", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Cache-Control", "no-cache")

	WritePageTemplate(w, &SqlteBackupsIndexPage{backups, prefix})
}

func (b *BackuperSqlite) viewBackupHandler(ctx context.Context, w http.ResponseWriter, user string, backupid int64,
) {
	conn, err := b.getConnection(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "sqlitebackup: get db connection failed", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	defer conn.Close()

	data, err := getFileFromDb(ctx, conn, backupid)
	switch {
	case errors.Is(err, ErrNotFound):
		slog.ErrorContext(ctx, "sqlitebackup: get file form db connection failed", "err", err)
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	case err != nil:
		slog.ErrorContext(ctx, "sqlitebackup: get file form db connection failed", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	case data.Username != user:
		slog.ErrorContext(ctx, "sqlitebackup: invalid user", "backup.user", data.Username, "user", user)
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data.Data)
	}
}

func (b *BackuperSqlite) restoreBackupHandler(ctx context.Context, w http.ResponseWriter, r *http.Request,
	root *os.Root, user string, backupid int64,
) {
	conn, err := b.getConnection(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "sqlitebackup: get db connection failed", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	defer conn.Close()

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024) //nolint:mnd // 1k
	fname := r.FormValue("filename")

	data, err := getFileFromDb(ctx, conn, backupid)
	switch {
	case errors.Is(err, ErrNotFound):
		slog.ErrorContext(ctx, "sqlitebackup: get file form db connection failed", "err", err)
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	case err != nil:
		slog.ErrorContext(ctx, "sqlitebackup: get file form db connection failed", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	case data.Username != user:
		slog.ErrorContext(ctx, "sqlitebackup: invalid user", "backup.user", data.Username, "user", user)
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	case r.Method == http.MethodPost && fname != "":
		if !isValidFilename(fname) {
			http.Error(w, http.StatusText(http.StatusBadRequest)+" - invalid filename", http.StatusBadRequest)

			return
		}

		slog.InfoContext(ctx, "restore backup file", "filename", fname, "backupid", backupid)

		if err := root.WriteFile(fname, data.Data, 0o660); err != nil { //nolint:mnd
			slog.ErrorContext(ctx, "write file error", "backup.filename", data.Filename, "err", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

			return
		}

		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusFound)

	default:
		WritePageTemplate(w, &SqlteBackupsRestorePage{data.Filename})
	}
}

func (b *BackuperSqlite) getConnection(ctx context.Context) (*sql.Conn, error) {
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "open db connection")
	}

	_, err = conn.ExecContext(ctx, `
PRAGMA page_size = 8192;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA temp_store = MEMORY;
`)
	if err != nil {
		conn.Close()

		return nil, errors.Wrap(err, "run db init conn script")
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
		return nil, errors.Wrap(err, "get users files")
	}

	defer rows.Close()

	for rows.Next() {
		uf := UserFile{} //nolint:exhaustruct

		if err := rows.Scan(&uf.Username, &uf.Filename); err != nil {
			return nil, errors.Wrap(err, "scan user files")
		}

		usersfiles = append(usersfiles, uf)
	}

	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "get users files")
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
		slog.DebugContext(ctx, "sqlitebackup: delete incr backups - not found")

		return nil
	} else if err != nil {
		return errors.Wrapf(err, "clean incr backup for user %q file %q", user, file)
	}

	slog.DebugContext(ctx, "sqlitebackup: delete incr backups", "min_id", backupid.Int64)

	res, err := tx.ExecContext(ctx, "DELETE FROM backups WHERE username=? AND filename=? AND isfull=0 and id<?",
		user, file, backupid)
	if err != nil {
		return errors.Wrapf(err, "clean incr backup for user %q file %q", user, file)
	}

	count, _ := res.RowsAffected()
	slog.DebugContext(ctx, "sqlitebackup: delete incr backups", "deleted", count)

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
		slog.DebugContext(ctx, "sqlitebackup: delete full backups - not found")

		return nil
	} else if err != nil {
		return errors.Wrapf(err, "clean full backup for user %q file %q", user, file)
	}

	slog.DebugContext(ctx, "sqlitebackup: delete full backups", "min_id", backupid.Int64)

	res, err := tx.ExecContext(ctx,
		"DELETE FROM backups as b WHERE username=? AND filename=? AND isfull=1 and id < ? "+
			" AND NOT EXISTS (SELECT NULL FROM backups b2 WHERE b2.parent = b.id) ",
		user, file, backupid)
	if err != nil {
		return errors.Wrapf(err, "clean full backup for user %q file %q", user, file)
	}

	count, _ := res.RowsAffected()
	slog.DebugContext(ctx, "sqlitebackup: delete full backups", "deleted", count)

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
			return errors.WithMessagef(err, "invalid policy value for number of full backups %q", fields[0])
		}

		if b.numFullBackups <= 0 {
			return fmt.Errorf("invalid policy value for number of full backups %q - must be greater than 0", fields[0])
		}
	}

	if len(fields) > 1 && fields[1] != "" {
		b.numIncrBackups, err = strconv.Atoi(fields[1])
		if err != nil {
			return errors.WithMessagef(err, "invalid policy value for number of incremental backups %q", fields[1])
		}

		if b.numIncrBackups < 0 {
			return fmt.Errorf("invalid policy value for number of incremental backups %q - must be greater or equal 0",
				fields[1])
		}
	}

	if len(fields) > 2 && fields[2] != "" {
		b.maxFullBackupAge, err = time.ParseDuration(fields[2])
		if err != nil {
			return errors.WithMessagef(err, "invalid policy value for full backup interval %q", fields[2])
		}

		if b.maxFullBackupAge < 0 {
			return fmt.Errorf( //nolint:err113
				"invalid policy value for full backup interval %q; must be greater than 0", fields[2],
			)
		}
	}

	slog.Info(
		fmt.Sprintf("sqlitebackup: backup policy: %d full backups, %d incremental backups, %s between full backups",
			b.numFullBackups, b.numIncrBackups, b.maxFullBackupAge),
	)

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
		return "", 0, errors.Wrap(err, "get last full content")
	}

	if compressed == 1 {
		con, err := decompressContent(content)
		if err != nil {
			return "", 0, err
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
			return err
		}

		compressed = 1
	}

	slog.DebugContext(ctx, "sqlitebackup: insert backup", "isfull", isfull,
		"compressed", compressed, "parent", parentID, "len", len(content))

	_, err := tx.ExecContext(ctx, `
			INSERT INTO backups (username, filename, ts, isfull, content, compressed, parent)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
		username, file, time.Now().Unix(), storefull, content, compressed, parentBackup)
	if err != nil {
		return errors.Wrap(err, "insert new content")
	}

	return nil
}

func (*BackuperSqlite) compressContent(content []byte) ([]byte, error) {
	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)

	_, err := zw.Write(content)
	zw.Close()

	if err != nil {
		return nil, errors.Wrap(err, "compress content")
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
		return nil, errors.Wrap(err, "decompress content")
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
		return nil, errors.Wrap(err, "open database file")
	}

	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "open db connection")
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
		return nil, errors.Wrap(err, "query backups")
	}

	defer rows.Close()

	backups := make([]SqliteBackup, 0)

	for rows.Next() {
		sb := SqliteBackup{} //nolint:exhaustruct

		var ts, isfull int64

		if err := rows.Scan(&sb.ID, &sb.Username, &ts, &sb.Filename, &isfull); err != nil {
			return nil, errors.Wrap(err, "scan")
		}

		sb.IsFull = isfull > 0
		sb.Timestamp = time.Unix(ts, 0)

		backups = append(backups, sb)
	}

	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "query backups")
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
		return nil, errors.Wrap(err, "open database file")
	}

	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "open db connection")
	}

	defer conn.Close()

	backup, err := getFileFromDb(ctx, conn, backupid)
	if err != nil {
		return nil, errors.WithMessagef(err, "get file from db")
	}

	return backup.Data, nil
}

//------------------------------------------------------------------------------

func getFileFromDb(ctx context.Context, conn *sql.Conn, backupid int64) (*SqliteBackup, error) { //nolint:cyclop,funlen
	slog.DebugContext(ctx, "sqlitebackup: load file from backup", "backupid", backupid)

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
		return nil, ErrNotFound
	} else if err != nil {
		return nil, errors.WithMessage(err, "get last full content")
	}

	if compressed == 1 {
		content, err = decompressContent(content)
		if err != nil {
			return nil, errors.WithMessage(err, "decompress content failed")
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
			return nil, errors.WithMessage(err, "decompress parent backup")
		}
	}

	dmp := diffmatchpatch.New()

	patches, err := dmp.PatchFromText(string(content))
	if err != nil {
		return nil, errors.Wrap(err, "path from content")
	}

	result, oks := dmp.PatchApply(patches, string(parentContent))
	for i, ok := range oks {
		if !ok {
			return nil, errors.Wrapf(err, "apply patch %d (%v)", i, patches[i])
		}
	}

	backup.Data = []byte(result)

	return &backup, nil
}
