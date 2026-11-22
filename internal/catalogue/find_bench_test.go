package catalogue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Find over a store of the user's size (9,199 songs, about 1.4 MB), as a press of a list parses it:
// every id of a whole-library Shuffle, and the ten of an album.
func benchFind(b *testing.B, want int) {
	dir := b.TempDir()
	st := Store{Card: dir, Path: filepath.Join(dir, "server-songs.tsv")}
	var sb strings.Builder
	sb.WriteString(StoreHead{Account: "a"}.line() + "\n")
	ids := map[string]bool{}
	for i := 0; i < 9199; i++ {
		s := Song{ID: fmt.Sprintf("id%020d", i), Title: fmt.Sprintf("A song title %d", i), Artist: "An Artist Name",
			AlbumArtist: "An Artist Name", Album: "An Album Title", AlbumID: fmt.Sprintf("al%020d", i/10), Year: 2020,
			Disc: 1, Track: i%10 + 1, Seconds: 240, Genre: "Hip Hop", Suffix: "flac", Size: 30000000}
		sb.WriteString(s.line() + "\n")
		if i < want {
			ids[s.ID] = true
		}
	}
	os.WriteFile(st.Path, []byte(sb.String()), 0o644)
	b.ReportMetric(float64(sb.Len())/1e6, "MB")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := st.Find("a", ids); len(got) != want {
			b.Fatalf("found %d of %d", len(got), want)
		}
	}
}

func BenchmarkFindAll9199(b *testing.B) { benchFind(b, 9199) }
func BenchmarkFindAlbum10(b *testing.B) { benchFind(b, 10) }
