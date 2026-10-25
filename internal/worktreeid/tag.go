package worktreeid

import "strings"

const MaxTagBytes = 64

const tagCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

// ValidTag reports whether tag may plan a fresh §9.4 worktree provision.
func ValidTag(tag string) bool {
	if tag == "" || len(tag) > MaxTagBytes {
		return false
	}
	return strings.IndexFunc(tag, func(r rune) bool {
		return !strings.ContainsRune(tagCharset, r)
	}) < 0
}

// ValidHistoricalTag reports whether tag has the identity shape accepted
// before the fresh-provision grammar existed.
func ValidHistoricalTag(tag string) bool {
	return tag != "" && !strings.Contains(tag, "/")
}
