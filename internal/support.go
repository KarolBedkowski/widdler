package internal

import (
	"fmt"
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
