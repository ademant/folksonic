package scanner

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/dhowden/tag"
)

// songTagValues returns the allowlisted tags of one file, keyed by upper-case
// name, with every value. FLAC and Ogg are read with the comment reader, which
// keeps repeated fields. Other formats use the tag library: ID3 user text frames
// (TXXX, keyed by description) and MP4 freeform atoms.
func songTagValues(path, suffix string, meta tag.Metadata, allow map[string]bool) map[string][]string {
	return collectTags(path, suffix, meta, allow, false)
}

// collectTags is songTagValues; with plain set it also reads plain ID3 text
// frames (TBPM) and MP4 atoms with string or integer values (tmpo), which
// the built-in tag readers need but song_tags does not expose.
func collectTags(path, suffix string, meta tag.Metadata, allow map[string]bool, plain bool) map[string][]string {
	out := map[string][]string{}
	add := func(name, value string) {
		value = strings.TrimSpace(value)
		if allow[name] && value != "" {
			out[name] = append(out[name], value)
		}
	}
	if comments, err := readVorbisComments(path, suffix); err == nil {
		for _, c := range comments {
			add(c.Name, c.Value)
		}
		return out
	}
	if meta == nil {
		return out
	}
	raw := meta.Raw()
	// Sorted, so the value order of repeated ID3 frames (TXXX, TXXX_0, ...) is
	// the same on every scan.
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := raw[k].(type) {
		case *tag.Comm:
			add(strings.ToUpper(v.Description), v.Text)
		case []string:
			for _, s := range v {
				add(strings.ToUpper(k), s)
			}
		case string:
			if plain {
				add(strings.ToUpper(k), v)
			}
		case int:
			if plain {
				add(strings.ToUpper(k), strconv.Itoa(v))
			}
		}
	}
	return out
}

// encodeSongTags stores the tags as a JSON object, or NULL when there are none.
func encodeSongTags(tags map[string][]string) sql.NullString {
	if len(tags) == 0 {
		return sql.NullString{}
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

// allowMap turns the configured names into a lookup set.
func allowMap(songTags []string) map[string]bool {
	m := make(map[string]bool, len(songTags))
	for _, n := range songTags {
		m[n] = true
	}
	return m
}
