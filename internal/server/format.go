package server

import (
	"path/filepath"
	"strings"

	"github.com/tsirysndr/tinysonic/internal/analysis"
)

// relativePath returns p relative to the music directory that contains it,
// with forward slashes, as Subsonic clients expect. Server paths are never
// exposed: a file outside every music directory (only possible after the
// config changed) is reported by its base name.
func relativePath(musicDirs []string, p string) string {
	if rel, ok := analysis.RelativePath(musicDirs, p); ok {
		return rel
	}
	return filepath.Base(p)
}

// DefaultIgnoredArticles is used when ignored_articles is not configured.
const DefaultIgnoredArticles = "The El La Las Le Les Los Il L' An A Die Das Ein"

// stripArticle removes one leading article from name for indexing and
// sorting. Articles ending in an apostrophe (L') match without a following
// space and with a typographic apostrophe too; others need a space after
// them. A name that is only an article is returned unchanged.
func stripArticle(name string, articles []string) string {
	for _, a := range articles {
		var rest string
		if base, ok := strings.CutSuffix(a, "'"); ok {
			for _, apo := range []string{"'", "’"} {
				pre := base + apo
				if len(name) > len(pre) && strings.EqualFold(name[:len(pre)], pre) {
					rest = name[len(pre):]
					break
				}
			}
		} else if len(name) > len(a) && name[len(a)] == ' ' && strings.EqualFold(name[:len(a)], a) {
			rest = name[len(a):]
		}
		if rest = strings.TrimLeft(rest, " "); rest != "" {
			return rest
		}
	}
	return name
}

// pruneNulls removes map entries whose value is nil, recursively. Subsonic
// optional fields without a value are omitted rather than sent as null,
// which strict client parsers reject.
func pruneNulls(v any) {
	switch t := v.(type) {
	case J:
		for k, e := range t {
			if e == nil {
				delete(t, k)
				continue
			}
			pruneNulls(e)
		}
	case []J:
		for _, e := range t {
			pruneNulls(e)
		}
	case []any:
		for _, e := range t {
			pruneNulls(e)
		}
	}
}
