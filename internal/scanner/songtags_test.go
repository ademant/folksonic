package scanner

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSongTagValuesFilterAndKeepOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.flac")
	data := flacWithComments("DANCE=Waltz", "DANCE=Mazurka", "USE=disco", "COMMENT=ignored")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got := songTagValues(path, "flac", nil, allowMap([]string{"DANCE", "USE"}))
	want := map[string][]string{"DANCE": {"Waltz", "Mazurka"}, "USE": {"disco"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEncodeSongTags(t *testing.T) {
	if got := encodeSongTags(nil); got.Valid {
		t.Fatalf("empty tags stored: %+v", got)
	}
	got := encodeSongTags(map[string][]string{"DANCE": {"Waltz"}})
	if !got.Valid || got.String != `{"DANCE":["Waltz"]}` {
		t.Fatalf("encoded %+v", got)
	}
}
