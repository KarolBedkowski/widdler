package internal

// config.go
// Copyright (C) 2026 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.

import (
	"encoding/csv"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/urfave/cli/v3"
	"gitlab.com/tozd/go/errors"
)

const (
	AuthBasic = "basic"
	AuthNone  = "none"
)

type Config struct {
	Auth       string
	HTpass     string
	WikisDir   string
	HTTPListen string
	TLSCert    string
	TLSKey     string

	BackupInterval int
	BackupMode     string
	BackupPolicy   string
	BackupCompress bool
	BackupDir      string
	BackupSqlite   string
}

func newConfigFromCmd(cmd *cli.Command) (*Config, error) {
	auth := cmd.String("auth")
	if auth == "" {
		auth = AuthNone
	}

	wikisdir, err := filepath.Abs(filepath.Clean(cmd.String("wikis")))
	if err != nil {
		return nil, errors.WithMessage(err, "invalid wikis directory")
	}

	if auth != AuthNone && auth != AuthBasic {
		return nil, errors.New("invalid auth type")
	}

	return &Config{
		Auth:           auth,
		HTpass:         filepath.Clean(cmd.String("htpass")),
		WikisDir:       wikisdir,
		HTTPListen:     cmd.String("http"),
		TLSCert:        cmd.String("tlscert"),
		TLSKey:         cmd.String("tlskey"),
		BackupInterval: cmd.Int("backup.interval"),
		BackupMode:     cmd.String("backup.mode"),
		BackupPolicy:   cmd.String("backup.policy"),
		BackupCompress: cmd.Bool("backup.compress"),
		BackupDir:      cmd.String("backup.dir"),
		BackupSqlite:   cmd.String("backup.sqlite_file"),
	}, nil
}

func (c *Config) userhome(user string) string {
	return filepath.Clean(path.Join(c.WikisDir, user))
}

// -------------------------------------------------------------------

func loadUsers(authmethod, htpassPath string) (map[string]string, error) {
	if authmethod == AuthNone {
		slog.Info("auth disabled")

		return nil, nil //nolint:nilnil
	}

	slog.Debug("loading passfile", "htpass", htpassPath, "auth", authmethod)

	p, err := os.Open(htpassPath)
	if err != nil {
		return nil, errors.WithMessagef(err, "open users file %q", htpassPath)
	}
	defer p.Close()

	ht := csv.NewReader(p)
	ht.Comma = ':'
	ht.Comment = '#'
	ht.TrimLeadingSpace = true

	entries, err := ht.ReadAll()
	if err != nil {
		return nil, errors.WithMessagef(err, "read users from %q", htpassPath)
	}

	users := make(map[string]string)

	for _, parts := range entries {
		if parts[0] == "" || parts[1] == "" { // skip entries with empty user and/or password
			continue
		}

		users[parts[0]] = parts[1]
	}

	return users, nil
}
