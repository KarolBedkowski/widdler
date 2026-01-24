package internal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"

	"github.com/urfave/cli/v3"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

// -------------------------------------------------------------------

var build = "dev"

// -------------------------------------------------------------------

func Main() { //nolint:funlen
	//nolint:exhaustruct
	cmd := &cli.Command{
		Name:    "widdler-ex",
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
				Usage:    "Output format of log messages. One of: [logfmt, json, tint]",
				Category: "Logging",
				Sources:  cli.EnvVars("WIDDLEREX_LOG_FORMAT"),
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "serve",
				Usage: "Start http server.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name: "wikis", Value: ".", Usage: "Directory of TiddlyWikis to serve over WebDAV.",
						Sources: cli.EnvVars("WIDDLEREX_WIKIS"),
					},
					&cli.StringFlag{
						Name: "http", Value: "localhost:8080", Usage: "Listen on.", Category: "Server",
						Sources: cli.EnvVars("WIDDLEREX_HTTP"),
					},
					&cli.StringFlag{
						Name: "tlscert", Usage: "TLS certificate.", Category: "Server",
						Sources: cli.EnvVars("WIDDLEREX_TLS_CERT"),
					},
					&cli.StringFlag{
						Name: "tlskey", Usage: "TLS key.", Category: "Server",
						Sources: cli.EnvVars("WIDDLEREX_TLS_KEY"),
					},
					&cli.StringFlag{
						Name:     "htpass",
						Value:    ".htpasswd",
						Usage:    "Path to .htpasswd file.",
						Category: "Authentication",
						Sources:  cli.EnvVars("WIDDLEREX_HTPASS"),
					},
					&cli.StringFlag{
						Name:     "auth",
						Value:    "none",
						Usage:    "Enable HTTP authentication (basic, header, none).",
						Category: "Authentication",
						Sources:  cli.EnvVars("WIDDLEREX_AUTH"),
					},
					&cli.StringFlag{
						Name:     "backup.dir",
						Value:    "backups",
						Usage:    "Directory for backups in user directory.",
						Category: "Backup",
						Sources:  cli.EnvVars("WIDDLEREX_BACKUP_DIR"),
					},
					&cli.BoolFlag{Name: "backup.compress", Value: false, Usage: "compress backup with GZIP."},
					&cli.StringFlag{
						Name:     "backup.policy",
						Value:    "7,7",
						Usage:    "Backup policy for selected mode; see README.md.",
						Category: "Backup",
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
						Name:     "backup.mode",
						Usage:    "Enable backup and select mode (file, sqlite)",
						Category: "Backup",
						Sources:  cli.EnvVars("WIDDLEREX_BACKUP_MODE"),
					},
					&cli.StringFlag{
						Name:     "backup.sqlite_file",
						Value:    "backup.sqlite",
						Usage:    "Backup file for 'sqlite' backup mode",
						Category: "Backup",
						Sources:  cli.EnvVars("WIDDLEREX_BACKUP_SQLITE_FILE"),
					},
				},
				Action: serverCmd,
			},
			{
				Name:  "gen-htpass",
				Usage: "Set or update user password in .htpasswd file.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "htpass",
						Value:    ".htpasswd",
						Usage:    "Path to .htpasswd file.",
						Required: true,
						Sources:  cli.EnvVars("WIDDLEREX_HTPASS"),
					},
				},
				Action: mainGenPass,
			},
			{
				Name:  "list-backups",
				Usage: "List created backups in sqlite file.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "file",
						Value:   "backup.sqlite",
						Usage:   "Path to backup file.",
						Sources: cli.EnvVars("WIDDLEREX_BACKUP_SQLITE_FILE"),
					},
					&cli.StringFlag{Name: "username", Usage: "Username for filter backups"},
				},
				Action: listSqliteBackupsCmd,
			},
			{
				Name:  "restore-backup",
				Usage: "Restore given backup from sqlite file.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "file",
						Value:   "backup.sqlite",
						Usage:   "Path to backup file.",
						Sources: cli.EnvVars("WIDDLEREX_BACKUP_SQLITE_FILE"),
					},
					&cli.Int64Flag{Name: "backupid", Usage: "Backup ID to restore", Required: true},
				},
				Action: restoreSqliteBackups,
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			setupLogging(cmd.String("log.level"), cmd.String("log.format"))

			return ctx, nil
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

// -------------------------------------------------------------------

func serverCmd(ctx context.Context, cmd *cli.Command) error {
	server := newServer(cmd)
	if err := server.Validate(); err != nil {
		return err
	}

	slog.Info("Wikis directory: " + server.davDir)
	slog.Info("Auth: " + server.auth)

	backuper, err := newBackuper(ctx, cmd)
	if err != nil {
		return err
	}

	if err := server.Start(ctx, &backuper); err != nil {
		return fmt.Errorf("serve error: %w", err)
	}

	return nil
}

// -------------------------------------------------------------------

func listSqliteBackupsCmd(ctx context.Context, cmd *cli.Command) error {
	dbfilename := cmd.String("file")
	if dbfilename == "" {
		return errors.New("missing database filename") //nolint:err113
	}

	username := cmd.String("username")

	res, err := ListSqliteBackups(ctx, dbfilename, username)
	if err != nil {
		return err
	}

	fmt.Printf("%4s | %-10s | %-30s | %-20s | %s\n", "ID", "User name", "Date time", "File name", "Kind")

	for _, r := range res {
		fmt.Println(r.ToString())
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

	fmt.Println(string(res))

	return nil
}

// -------------------------------------------------------------------

func prompt(prompt string, secure bool) (string, error) {
	fmt.Print(prompt)

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

	fmt.Printf("Added %q to %q\n", user, passPath)

	return nil
}

// -------------------------------------------------------------------
