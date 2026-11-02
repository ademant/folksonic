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
		if len(id) != 3+16 {
			t.Errorf("unexpected ID length for %q", id)
		}
	}
}
