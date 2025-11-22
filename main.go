package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
	"suah.dev/protect"
)

// -------------------------------------------------------------------

var build = "dev"

type Configuration struct {
	auth       string
	davDir     string
	listen     string
	passPath   string
	tlsCert    string
	tlsKey     string
	fullListen string
}

func (c *Configuration) validate() error {
	var err error

	c.davDir, err = filepath.Abs(c.davDir)
	if err != nil {
		return fmt.Errorf("check wikis dir %q error: %w", c.davDir, err)
	}

	c.passPath = filepath.Clean(c.passPath)

	if c.tlsCert != "" && c.tlsKey != "" {
		c.fullListen = "https://" + c.listen
	} else {
		c.fullListen = "http://" + c.listen
	}

	return nil
}

func loadConfiguration(cmd *cli.Command) (*Configuration, *Backuper, error) {
	conf := Configuration{ //nolint:exhaustruct
		davDir:   cmd.String("wikis"),
		listen:   cmd.String("http"),
		tlsCert:  cmd.String("tlscert"),
		tlsKey:   cmd.String("tlskey"),
		passPath: cmd.String("htpass"),
		auth:     cmd.String("auth"),
	}

	backuper := Backuper{ //nolint:exhaustruct
		backupDir:   cmd.String("backup.dir"),
		compress:    cmd.Bool("backup.compress"),
		keepDaily:   cmd.Int("backup.keep_daily"),
		keepOnWrite: cmd.Int("backup.keep_on_write"),
		interval:    cmd.Int("backup.interval"),
		mode:        cmd.String("backup.mode"),
		sqliteFile:  cmd.String("backup.sqliteFile"),
	}

	logLevel := cmd.String("log.level")
	logFormat := cmd.String("log.format")

	setupLogging(&logLevel, &logFormat)

	if err := conf.validate(); err != nil {
		return nil, nil, err
	}

	slog.Info("Wikis directory: " + conf.davDir)
	slog.Info("Auth: " + conf.auth)

	return &conf, &backuper, nil
}

func secure(path ...string) string {
	pledges := "stdio wpath rpath cpath tty inet dns unveil"
	// These are OpenBSD specific protections used to prevent unnecessary file access.
	for _, p := range path {
		_ = protect.Unveil(p, "rwc")
	}

	_ = protect.Unveil("/etc/ssl/cert.pem", "r")
	_ = protect.Unveil("/etc/resolv.conf", "r")
	_ = protect.Pledge(pledges)

	return pledges
}

// -------------------------------------------------------------------

func prompt(prompt string, secure bool) (string, error) {
	fmt.Print(prompt) //nolint:forbidigo

	var input string

	if secure {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return "", fmt.Errorf("read password error: %w", err)
		}

		input = string(b)
	} else if _, err := fmt.Scanln(&input); err != nil {
		return "", fmt.Errorf("read stdin error: %w", err)
	}

	if input == "" {
		return "", fmt.Errorf("empty %q", prompt) //nolint:err113
	}

	return input, nil
}

func mainGenPass(ctx context.Context, cmd *cli.Command) error {
	_ = ctx
	passPath := cmd.String("htpass")

	user, err := prompt("Username: ", false)
	if err != nil {
		return fmt.Errorf("get username error: %w", err)
	}

	pass, err := prompt("Password: ", true)
	if err != nil {
		return fmt.Errorf("get password error: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pass), 11) //nolint:mnd
	if err != nil {
		return fmt.Errorf("hash password error: %w", err)
	}

	const filePerm = 0o600

	f, err := os.OpenFile(passPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("open passfile %q error: %w", passPath, err)
	}

	if _, err := fmt.Fprintf(f, "%s:%s\n", user, hash); err != nil {
		return fmt.Errorf("write to passfile error: %w", err)
	}

	if err = f.Close(); err != nil {
		return fmt.Errorf("close passfile error: %w", err)
	}

	fmt.Printf("Added %q to %q\n", user, passPath) //nolint:forbidigo

	return nil
}

// -------------------------------------------------------------------

func main() { //nolint:funlen
	//nolint:exhaustruct
	cmd := &cli.Command{
		Name:    "widdler",
		Version: build,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "log.level",
				Value: "info",
				Usage: "Only log messages with the given severity or above. One of: [debug, info, warn, error]",
			},
			&cli.StringFlag{
				Name:  "log.format",
				Value: "",
				Usage: "Output format of log messages. One of: [logfmt, json, tint]",
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "serve",
				Usage: "start server",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "wikis", Value: ".", Usage: "Directory of TiddlyWikis to serve over WebDAV."},
					&cli.StringFlag{Name: "http", Value: "localhost:8080", Usage: "Listen on"},
					&cli.StringFlag{Name: "tlscert", Usage: "TLS certificate."},
					&cli.StringFlag{Name: "tlskey", Usage: "TLS key."},
					&cli.StringFlag{Name: "htpass", Value: ".htpasswd", Usage: "Path to .htpasswd file.."},
					&cli.StringFlag{
						Name:  "auth",
						Value: "none",
						Usage: "Enable HTTP Basic Authentication (basic, none, header).",
					},
					&cli.StringFlag{
						Name:  "backup.dir",
						Value: "backups",
						Usage: "Directory for backups in user directory.",
					},
					&cli.BoolFlag{Name: "backup.compress", Value: false, Usage: "GZIP backup files."},
					&cli.IntFlag{
						Name:  "backup.keep_daily",
						Value: 0,
						Usage: "If > 0 keep given number of daily backups.",
					},
					&cli.IntFlag{
						Name:  "backup.keep_on_write",
						Value: 0,
						Usage: "If > 0 keep given number of backup created on write.",
					},
					&cli.IntFlag{
						Name:  "backup.interval",
						Value: 30, //nolint:mnd
						Usage: "Minimal time between backups (in seconds)",
					},
					&cli.StringFlag{Name: "backup.mode", Value: "", Usage: "Backup mode (file, git, git-once)"},
					&cli.StringFlag{
						Name:  "backup.sqliteFile",
						Value: "backup.sqlite",
						Usage: "Backup file for sqlite-mode",
					},
				},
				Action: serverCmd,
			},
			{
				Name: "genHtpass",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "htpass",
						Value:    ".htpasswd",
						Usage:    "Path to .htpasswd file.",
						Required: true,
					},
				},
				Action: mainGenPass,
			},
			{
				Name: "list-backups",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "file",
						Value:    "backup.sqlite",
						Usage:    "Path to backup file.",
						Required: true,
					},
					&cli.StringFlag{Name: "username", Usage: "Username for filter backups"},
				},
				Action: listSqliteBackups,
			},
			{
				Name: "restore-backup",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "file",
						Value:    "backup.sqlite",
						Usage:    "Path to backup file.",
						Required: true,
					},
					&cli.Int64Flag{Name: "backupid", Usage: "Backup ID to restore"},
				},
				Action: restoreSqliteBackups,
			},
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

// -------------------------------------------------------------------

func serverCmd(ctx context.Context, cmd *cli.Command) error {
	conf, backuper, err := loadConfiguration(cmd)
	if err != nil {
		return err
	}

	pledges := secure(conf.davDir, conf.passPath)
	pledges, _ = protect.ReducePledges(pledges, "tty")

	// drop to only read on passPath
	_ = protect.Unveil(conf.passPath, "r")
	_, _ = protect.ReducePledges(pledges, "unveil")

	backuper.davDir = conf.davDir

	mainServer(ctx, conf, backuper)

	return nil
}

// -------------------------------------------------------------------

func listSqliteBackups(ctx context.Context, cmd *cli.Command) error {
	dbfilename := cmd.String("file")
	if dbfilename == "" {
		return errors.New("missing database filename") //nolint:err113
	}

	username := cmd.String("username")

	res, err := ListSqliteBackups(ctx, dbfilename, username)
	if err != nil {
		return err
	}

	for _, r := range res {
		fmt.Println(r.ToString()) //nolint:forbidigo
	}

	return nil
}

// -------------------------------------------------------------------

func restoreSqliteBackups(ctx context.Context, cmd *cli.Command) error {
	dbfilename := cmd.String("file")
	if dbfilename == "" {
		return errors.New("missing database filename") //nolint:err113
	}

	backupid := cmd.Int64("backupid")
	if backupid <= 0 {
		return errors.New("missing or invalid backupid") //nolint:err113
	}

	res, err := RestoreSqliteBackup(ctx, dbfilename, backupid)
	if err != nil {
		return err
	}

	fmt.Println(string(res)) //nolint:forbidigo

	return nil
}

// -------------------------------------------------------------------
