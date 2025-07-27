package main

//
// backup_test.go
// Copyright (C) 2025 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"slices"
	"testing"
	"time"
)

func TestSelectToDel1(t *testing.T) {
	files := []string{
		"b--20250720_123456.htm",
		"b--20250720_123000.html",
		"b--20250720_122030.htm.gz",
		"b--20250720_122010.html.gz",
		"b--20250720_113456.htm",
		"b--20250720_113000.html",
		"b--20250720_112010.html.gz",
		"b--20250720_112030.htm.gz",
		"b--20250719_093456.htm",
		"b--20250719_093000.html",
		"b--20250718_093456.htm",
		"b--20250717_073000.html",
		"b--20250716_063456.htm",
		"b--20250715_053000.html",
	}

	expToDel := []string{
		"b--20250720_122010.html.gz",
		"b--20250720_113456.htm",
		"b--20250720_113000.html",
		"b--20250720_112010.html.gz",
		"b--20250720_112030.htm.gz",
		"b--20250719_093000.html",
		"b--20250715_053000.html",
	}

	now, _ := time.Parse("2006-01-02", "2025-07-20")
	toDel := selectFilesToDel(files, now, 3, 4)

	if len(toDel) != len(expToDel) {
		t.Fatalf("invalid toDel list: %v", toDel)
	}

	for _, exp := range expToDel {
		if !slices.Contains(toDel, exp) {
			t.Fatalf("missing %q in toDel %v", exp, toDel)
		}
	}
}

func TestSelectToDel2(t *testing.T) {
	files := []string{
		"b--20250720_123456.htm",
		"b--20250720_123000.html",
		"b--20250720_122030.htm.gz",
		"b--20250720_122010.html.gz",
		"b--20250720_113456.htm",
		"b--20250720_113000.html",
		"b--20250720_112010.html.gz",
		"b--20250720_112030.htm.gz",
		"b--20250719_093456.htm",
		"b--20250719_093000.html",
		"b--20250718_093456.htm",
		"b--20250717_073000.html",
		"b--20250716_063456.htm",
		"b--20250715_053000.html",
	}

	expToDel := []string{
		"b--20250719_093456.htm",
		"b--20250719_093000.html",
		"b--20250718_093456.htm",
		"b--20250717_073000.html",
		"b--20250716_063456.htm",
		"b--20250715_053000.html",
	}

	now, _ := time.Parse("2006-01-02", "2025-07-20")
	toDel := selectFilesToDel(files, now, 100, 0)

	if len(toDel) != len(expToDel) {
		t.Fatalf("invalid toDel list: %v", toDel)
	}

	for _, exp := range expToDel {
		if !slices.Contains(toDel, exp) {
			t.Fatalf("missing %q in toDel %v", exp, toDel)
		}
	}
}

func TestSelectToDel3(t *testing.T) {
	files := []string{
		"b--20250720_123456.htm",
		"b--20250720_123000.html",
		"b--20250720_122030.htm.gz",
		"b--20250720_122010.html.gz",
		"b--20250720_113456.htm",
		"b--20250720_113000.html",
		"b--20250720_112010.html.gz",
		"b--20250720_112030.htm.gz",
		"b--20250719_093456.htm",
		"b--20250719_093000.html",
		"b--20250718_093456.htm",
		"b--20250717_073000.html",
		"b--20250716_063456.htm",
		"b--20250715_053000.html",
	}

	expToDel := []string{
		"b--20250720_123000.html",
		"b--20250720_122030.htm.gz",
		"b--20250720_122010.html.gz",
		"b--20250720_113456.htm",
		"b--20250720_113000.html",
		"b--20250720_112010.html.gz",
		"b--20250720_112030.htm.gz",
		"b--20250719_093000.html",
	}

	now, _ := time.Parse("2006-01-02", "2025-07-20")
	toDel := selectFilesToDel(files, now, 1, 100)

	if len(toDel) != len(expToDel) {
		t.Fatalf("invalid toDel list: %v", toDel)
	}

	for _, exp := range expToDel {
		if !slices.Contains(toDel, exp) {
			t.Fatalf("missing %q in toDel %v", exp, toDel)
		}
	}
}

func TestGroupFiles1(t *testing.T) {
	files := []string{
		"b--20250720_123456.htm",
		"ba--20250720_123000.html",
		"bb--20250720_122030.htm.gz",
		"bc--20250720_122010.html.gz",
		"b-20250720_113456.htm", // ignored
		"ba--20250720_113000.html",
		"bb--20250720_112010.html.gz",
	}

	expected := map[string][]string{
		"b":  {"b--20250720_123456.htm"},
		"ba": {"ba--20250720_123000.html", "ba--20250720_113000.html"},
		"bb": {"bb--20250720_122030.htm.gz", "bb--20250720_112010.html.gz"},
		"bc": {"bc--20250720_122010.html.gz"},
	}

	groups := groupFilesByPrefix(files)

	if len(expected) != len(groups) {
		t.Fatalf("invalid result: %v", groups)
	}

	for g, exp := range expected {
		if slices.Compare(exp, groups[g]) != 0 {
			t.Fatalf("for %q expected %#v got %#v", g, exp, groups[g])
		}
	}
}
