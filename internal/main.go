package internal

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/oklog/run"
	"github.com/urfave/cli/v3"
	"gitlab.com/tozd/go/errors"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

// -------------------------------------------------------------------

var (
	build   = "dev"
	devmode = false
)

const (
	DefaultBackupFilename = "backup.sqlite"
	CliCategoryServer     = "Server"
	CliCategoryBackup     = "Backup"
)

// -------------------------------------------------------------------

func Main() { //nolint:funlen
	//nolint:exhaustruct
	cmd := &cli.Command{
		Name:    "widdler-ex",
		Usage:   "Web/dav server for TiddlyWiki wikis",
		Version: build,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "log.level",
				Value:    "info",
				Usage:    "Only log messages with the given severity or above. One of: [debug, info, warn, error]",
				Category: "Logging",
				Sources:  cli.EnvVars("WIDDLEREX_LOG_LEVEL"),
			},
			&cli.StringFlag{
				Name:     "log.format",
				Value:    "",
				Usage:    "The output format of log messages. One of: [logfmt, json, tint]",
				Category: "Logging",
				Sources:  cli.EnvVars("WIDDLEREX_LOG_FORMAT"),
			},
			&cli.BoolFlag{
				Name:    "devmode",
				Value:   false,
				Hidden:  true,
				Sources: cli.EnvVars("WIDDLEREX_DEVMODE"),
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "serve",
				Usage: "Start an HTTP/HTTPS server.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name: "wikis", Value: ".", Usage: "Directory of TiddlyWikis to serve over WebDAV.",
						Sources:   cli.EnvVars("WIDDLEREX_WIKIS"),
						Validator: dirMustExistsValidator,
					},
					&cli.StringFlag{
						Name: "http", Value: "localhost:8080", Usage: "Listen on.", Category: CliCategoryServer,
						Sources: cli.EnvVars("WIDDLEREX_HTTP"),
					},
					&cli.StringFlag{
						Name: "tlscert", Usage: "TLS certificate.", Category: CliCategoryServer,
						Sources:   cli.EnvVars("WIDDLEREX_TLS_CERT"),
						Validator: fileMustExistsValidator,
					},
					&cli.StringFlag{
						Name: "tlskey", Usage: "TLS key.", Category: CliCategoryServer,
						Sources:   cli.EnvVars("WIDDLEREX_TLS_KEY"),
						Validator: fileMustExistsValidator,
					},
					&cli.StringFlag{
						Name:      "htpass",
						Value:     ".htpasswd",
						Usage:     "Path to .htpasswd file.",
						Category:  "Authentication",
						Sources:   cli.EnvVars("WIDDLEREX_HTPASS"),
						Validator: fileMustExistsValidator,
					},
					&cli.StringFlag{
						Name:      "auth",
						Value:     "none",
						Usage:     "Enable HTTP authentication (basic, none).",
						Category:  "Authentication",
						Sources:   cli.EnvVars("WIDDLEREX_AUTH"),
						Validator: argChoiceValidator("basic", "none", ""),
					},
					&cli.StringFlag{
						Name:      "backup.dir",
						Value:     "backups",
						Usage:     "Directory for backups in user directory.",
						Category:  CliCategoryBackup,
						Sources:   cli.EnvVars("WIDDLEREX_BACKUP_DIR"),
						Validator: dirMustExistsValidator,
					},
					&cli.BoolFlag{Name: "backup.compress", Value: false, Usage: "compress backup with GZIP."},
					&cli.StringFlag{
						Name:     "backup.policy",
						Value:    "7,7",
						Usage:    "Backup policy for selected mode; see README.md.",
						Category: CliCategoryBackup,
						Sources:  cli.EnvVars("WIDDLEREX_BACKUP_POLICY"),
					},
					&cli.IntFlag{
						Name:     "backup.interval",
						Value:    30, //nolint:mnd
						Usage:    "Minimal time between backups (in seconds)",
						Category: "Backup",
						Sources:  cli.EnvVars("WIDDLEREX_BACKUP_INTERVAL"),
					},
					&cli.StringFlag{
						Name:      "backup.mode",
						Usage:     "Enable backup and select mode (file, sqlite)",
						Category:  "Backup",
						Sources:   cli.EnvVars("WIDDLEREX_BACKUP_MODE"),
						Validator: argChoiceValidator("file", "sqlite", ""),
					},
					&cli.StringFlag{
						Name:      "backup.sqlite_file",
						Value:     DefaultBackupFilename,
						Usage:     "Backup file for 'sqlite' backup mode",
						Category:  CliCategoryBackup,
						Sources:   cli.EnvVars("WIDDLEREX_BACKUP_SQLITE_FILE"),
						Validator: fileMustExistsValidator,
					},
				},
				Action: serverCmd,
			},
			{
				Name:  "gen-htpass",
				Usage: "Set or update user password in .htpasswd file.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "htpass",
						Value:   ".htpasswd",
						Usage:   "Path to .htpasswd file.",
						Sources: cli.EnvVars("WIDDLEREX_HTPASS"),
					},
				},
				Action: mainGenPass,
			},
			{
				Name:  "list-backups",
				Usage: "List created backups in SQLite file.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:      "file",
						Value:     DefaultBackupFilename,
						Usage:     "Path to backup file.",
						Sources:   cli.EnvVars("WIDDLEREX_BACKUP_SQLITE_FILE"),
						Validator: fileMustExistsValidator,
					},
					&cli.StringFlag{Name: "username", Usage: "Username for filter backups"},
				},
				Action: listSqliteBackupsCmd,
			},
			{
				Name:  "restore-backup",
				Usage: "Restore a given backup from SQLite file.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:      "file",
						Value:     DefaultBackupFilename,
						Usage:     "Path to backup file.",
						Sources:   cli.EnvVars("WIDDLEREX_BACKUP_SQLITE_FILE"),
						Validator: fileMustExistsValidator,
					},
					&cli.Int64Flag{Name: "backupid", Usage: "Backup ID to restore", Required: true},
				},
				Action: restoreSqliteBackups,
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			setupLogging(cmd.String("log.level"), cmd.String("log.format"))

			devmode = cmd.Bool("devmode")
			if devmode {
				slog.Warn("devmode enabled")
			}

			return ctx, nil
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		slog.Error("run failed", "err", err)

		if pub := Public(err); pub != "" {
			fmt.Fprintf(os.Stdout, "Error: %s", pub)
		} else {
			fmt.Fprintf(os.Stdout, "Error: %s", err.Error())
		}

		os.Exit(1)
	}
}

// -------------------------------------------------------------------

func serverCmd(ctx context.Context, cmd *cli.Command) error {
	config, err := newConfigFromCmd(cmd)
	if err != nil {
		return errors.WithMessage(err, "invalid configuration")
	}

	if err := ensureDirExists(config.WikisDir); err != nil {
		return errors.WithMessagef(err, "wikis dir %q check error", config.WikisDir)
	}

	users, err := loadUsers(config.Auth, config.HTpass)
	if err != nil {
		return err
	}

	usernames := slices.Collect(maps.Keys(users))

	if err := ensureUserHomesExists(config.WikisDir, usernames); err != nil {
		return err
	}

	backuper, err := newBackuper(ctx, config)
	if err != nil {
		return errors.WithMessage(err, "initiate backup")
	}

	server := newServer(config, users, backuper)
	if err := server.StartListen(ctx); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL)
	runGroup := run.Group{}

	if backuper.enabled {
		runGroup.Add(func() error { return backuper.Run(ctx, usernames) }, func(error) {})
	}

	runGroup.Add(func() error { return server.Run(ctx) }, server.Stop)
	runGroup.Add(func() error {
		<-ctx.Done()

		return nil
	}, func(error) { cancel() })

	if err := runGroup.Run(); err != nil {
		return errors.WithMessage(err, "run failed")
	}

	slog.InfoContext(ctx, "stopped")

	return nil
}

// -------------------------------------------------------------------

func listSqliteBackupsCmd(ctx context.Context, cmd *cli.Command) error {
	dbfilename := cmd.String("file")
	if dbfilename == "" {
		return errors.New("missing database filename")
	}

	username := cmd.String("username")

	res, err := ListSqliteBackups(ctx, dbfilename, username)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "%4s | %-10s | %-30s | %-20s | %s\n", "ID", "User name", "Date time", "File name", "Kind")

	for _, r := range res {
		fmt.Fprintln(os.Stdout, r.ToString())
	}

	return nil
}

// -------------------------------------------------------------------

func restoreSqliteBackups(ctx context.Context, cmd *cli.Command) error {
	dbfilename := cmd.String("file")
	if dbfilename == "" {
		return errors.New("missing database filename")
	}

	backupid := cmd.Int64("backupid")
	if backupid <= 0 {
		return errors.New("missing or invalid backupid")
	}

	res, err := RestoreSqliteBackup(ctx, dbfilename, backupid)
	if err != nil {
		return errors.WithMessage(err, "restore")
	}

	fmt.Fprintln(os.Stdout, string(res))

	return nil
}

// -------------------------------------------------------------------

func prompt(prompt string, secure bool) (string, error) {
	fmt.Fprint(os.Stdout, prompt)

	var input string

	if secure {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return "", errors.WithMessage(err, "read password")
		}

		input = string(b)
	} else if _, err := fmt.Scanln(&input); err != nil {
		return "", errors.WithMessage(err, "read stdin")
	}

	if input == "" {
		return "", errors.New("no data entered")
	}

	return input, nil
}

func mainGenPass(ctx context.Context, cmd *cli.Command) error {
	_ = ctx

	passPath := cmd.String("htpass")
	if passPath == "" {
		return errors.New("missing htpass")
	}

	user, err := prompt("Username: ", false)
	if err != nil {
		return errors.WithMessage(err, "prompt username")
	}

	pass, err := prompt("Password: ", true)
	if err != nil {
		return errors.WithMessage(err, "prompt password")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pass), 11) //nolint:mnd
	if err != nil {
		return errors.WithMessage(err, "hash password")
	}

	f, err := os.OpenFile(passPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, defaultFilePerm)
	if err != nil {
		return errors.WithMessage(err, "open passfile")
	}

	if _, err := fmt.Fprintf(f, "%s:%s\n", user, hash); err != nil {
		return errors.WithMessage(err, "write to passfile")
	}

	if err = f.Close(); err != nil {
		return errors.WithMessage(err, "close passfile")
	}

	fmt.Fprintf(os.Stdout, "Added %q to %q\n", user, passPath)

	return nil
}

// -------------------------------------------------------------------

func argChoiceValidator(values ...string) func(v string) error {
	return func(v string) error {
		if slices.Contains(values, v) {
			return nil
		}

		return errors.Errorf("accepted values: %v", values)
	}
}

func fileMustExistsValidator(v string) error {
	if v == "" {
		return nil
	}

	s, err := os.Stat(v)
	if s == nil || errors.Is(err, os.ErrNotExist) {
		return errors.New("file do not exists")
	}

	if s.IsDir() {
		return errors.Errorf("%q is a directory", v)
	}

	return nil
}

func dirMustExistsValidator(v string) error {
	if v == "" {
		return nil
	}

	s, err := os.Stat(v)
	if s == nil || errors.Is(err, os.ErrNotExist) {
		return errors.New("file do not exists")
	}

	if !s.IsDir() {
		return errors.Errorf("%q is not a directory", v)
	}

	return nil
}

// -------------------------------------------------------------------

func ensureUserHomesExists(base string, usernames []string) error {
	if len(usernames) == 0 {
		return nil
	}

	root, err := os.OpenRoot(base)
	if err != nil {
		return errors.WithMessagef(err, "open root %q error", base)
	}

	defer root.Close()

	for _, u := range usernames {
		if err := ensureDirExistsInRoot(root, u); err != nil {
			return errors.WithMessagef(err, "user home dir %q check error", u)
		}
	}

	return nil
}
