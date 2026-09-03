package acquisition

import (
	"path/filepath"
	"strings"
)

// NormalizeLocalPath converts a Windows drive path received by a browser or
// another client into the conventional WSL mount path. Linux paths are
// returned unchanged apart from slash normalization by the caller's usual
// filepath handling.
//
// Examples:
//
//	E:\\Movies\\movie.mp4 -> /mnt/e/Movies/movie.mp4
//	C:/Movies/movie.mp4    -> /mnt/c/Movies/movie.mp4
func NormalizeLocalPath(value string) string {
	path := strings.TrimSpace(value)
	if path == "" {
		return ""
	}
	if len(path) >= 3 && isASCIIAlpha(path[0]) && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		rest := strings.ReplaceAll(path[2:], "\\", "/")
		return "/mnt/" + strings.ToLower(path[:1]) + rest
	}
	return filepath.Clean(path)
}

func isASCIIAlpha(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}
