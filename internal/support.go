package internal

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

//
// support.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

func formatDate(t time.Time) string { return t.Local().Format(time.DateTime) } //nolint:gosmopolitan

var (
	sizesName = []string{"B", "kB", "MB", "GB"}
	sizesDiv  = []int64{1, 1024, 1048576, 1073741824}
)

func formatSize(size int64) string {
	if size < 1024 { //nolint:mnd
		return fmt.Sprintf("%0d B", size)
	}

	i := len(sizesDiv) - 1

	for i > 0 && sizesDiv[i] > size {
		i--
	}

	return fmt.Sprintf("%0.2f %s", float64(size)/float64(sizesDiv[i]), sizesName[i])
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}

	return v
}

func isValidFilename(name string) bool {
	if name == "" || name[0] == '.' {
		return false
	}

	if strings.ContainsAny(name, `/\:><"|?*`) {
		return false
	}

	if slices.Contains([]string{
		"CON", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
		"PRN", "AUX", "NUL",
	}, name) {
		return false
	}

	return true
}
