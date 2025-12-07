package internal

//
// templates.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"embed"
	"fmt"
	"html/template"
	"time"
)

//go:embed tmpl/*.tmpl
var templatesFS embed.FS
var appTemplates *template.Template

func init() {
	funcMap := template.FuncMap{
		"formatDate": func(t time.Time) string { return t.Local().Format(time.DateTime) }, //nolint:gosmopolitan
		"formatSize": formatSize,
	}

	appTemplates = template.Must(template.New("").Funcs(funcMap).ParseFS(templatesFS, "tmpl/*.tmpl"))
}

var (
	sizesName = []string{"B", "B", "kB", "MB", "GB"}
	sizesDiv  = []int64{0, 1024, 1048576, 1073741824, 1099511627776}
)

func formatSize(size int64) string {
	dividor := 0

	for i, d := range sizesDiv {
		dividor = i

		if size < d {
			break
		}
	}

	return fmt.Sprintf("%0.2f %s", float64(size)/float64(sizesDiv[dividor-1]), sizesName[dividor])
}
