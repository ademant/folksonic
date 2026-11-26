package scanner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Music folder containment (#177). Symlinks inside a music folder are
// followed, but only while they lead somewhere inside that same folder: a
// link to /etc, or a file link named like audio pointing at /etc/passwd, is
// neither indexed nor served. The scan and the watcher check with Contained
// before they index a file; reads that serve or copy a file go through
// OpenContained, which uses os.Root, so a link changed after the scan cannot
// lead the server out of the folder either.

// ErrOutsideMusic is returned for a path that is not inside a music folder
// once its symlinks are resolved.
var ErrOutsideMusic = errors.New("outside the music folders")

// rootsOf returns the roots that contain path by name, before any symlink is
// resolved. Nested roots can both contain a path.
func rootsOf(roots []string, path string) []string {
	var out []string
	for _, r := range roots {
		if within(absClean(r), absClean(path)) {
			out = append(out, r)
		}
	}
	return out
}

func absClean(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}

// within reports whether path is root or lies below it. Both are cleaned
// absolute paths.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Contained reports whether path, with every symlink resolved, is inside a
// music folder that contains it by name: the same rule os.Root applies.
func Contained(roots []string, path string) bool {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	real = absClean(real)
	for _, r := range rootsOf(roots, path) {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		if within(absClean(rr), real) {
			return true
		}
	}
	return false
}

// OpenContained opens path for reading if Contained accepts it. The path is
// resolved first and the link-free result is opened through os.Root of the
// music folder, so a symlink swapped in between cannot lead outside it
// either (os.Root refuses that, and refuses absolute links, which is why the
// links are resolved here). It fails with ErrOutsideMusic when no folder
// contains path.
func OpenContained(roots []string, path string) (*os.File, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	real = absClean(real)
	last := ErrOutsideMusic
	for _, r := range rootsOf(roots, path) {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			last = err
			continue
		}
		rr = absClean(rr)
		if !within(rr, real) {
			continue
		}
		rel, err := filepath.Rel(rr, real)
		if err != nil {
			continue
		}
		root, err := os.OpenRoot(rr)
		if err != nil {
			last = err
			continue
		}
		f, err := root.Open(rel)
		root.Close() // the opened file stays valid
		if err == nil {
			return f, nil
		}
		last = err
	}
	return nil, last
}
