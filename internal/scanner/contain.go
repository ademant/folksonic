package scanner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutsideMusic is returned for a path outside the configured music folder
// after resolving symlinks.
var ErrOutsideMusic = errors.New("outside the music folder")

func absClean(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Contained reports whether path, with every symlink resolved, is inside root.
func Contained(root, path string) bool {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	return within(absClean(resolvedRoot), absClean(real))
}

// OpenContained opens path only when it resolves inside the music folder.
// os.Root prevents a symlink swapped after evaluation from escaping.
func OpenContained(root, path string) (*os.File, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	real = absClean(real)
	resolvedRoot = absClean(resolvedRoot)
	if !within(resolvedRoot, real) {
		return nil, ErrOutsideMusic
	}
	rel, err := filepath.Rel(resolvedRoot, real)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(resolvedRoot)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Open(rel)
}
