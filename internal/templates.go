package internal

//
// templates.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"embed"
	"html/template"
)

//go:embed tmpl/*.tmpl
var templatesFS embed.FS
var appTemplates *template.Template

func init() {
	appTemplates = template.Must(template.ParseFS(templatesFS, "tmpl/*.tmpl"))
}
