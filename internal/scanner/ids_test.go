package scanner

import "testing"

func TestIDsAreStableAndPrefixed(t *testing.T) {
	if ArtistID("Radiohead") != ArtistID("radiohead") {
		t.Error("artist ID must be case-insensitive")
	}
	if AlbumID("A", "B", 1999) == AlbumID("A", "B", 2000) {
		t.Error("album ID must depend on year")
	}
	if SongID("/a.mp3") == SongID("/b.mp3") {
		t.Error("song ID must depend on path")
	}
	for _, id := range []string{ArtistID("x"), AlbumID("x", "y", 0), SongID("/x")} {
		if len(id) != 3+32 {
			t.Errorf("unexpected ID length for %q", id)
		}
	}
}

// IDs are bookmarks for clients, so their exact values are pinned. The
// expected values are the first 32 hex characters of the SHA-256 of the
// input, computed outside the Go code.
func TestIDsAreFixedValues(t *testing.T) {
	if got, want := SongID("/x"), "so-b3d1db318671a024a7e4b433389f8820"; got != want {
		t.Errorf("SongID = %q, want %q", got, want)
	}
	if got, want := ArtistID("Radiohead"), "ar-3f3371c759cd4d39b5d9ac571fdde100"; got != want {
		t.Errorf("ArtistID = %q, want %q", got, want)
	}
}
