package main

import (
	"path/filepath"
	"sort"
	"strconv"
)

func filepathJoin(a, b string) string { return filepath.Join(a, b) }
func atoi(s string) int                { n, _ := strconv.Atoi(s); return n }
func sortStrings(s []string)           { sort.Strings(s) }
