package importer

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrNoSpecs distinguishes an empty corpus (including deletion of its final
// file) from an unreadable or malformed corpus.
var ErrNoSpecs = errors.New("no spec files")

func excluded(path string, paths []string) bool {
	path, _ = filepath.Abs(path)
	for _, p := range paths {
		p, _ = filepath.Abs(p)
		if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
