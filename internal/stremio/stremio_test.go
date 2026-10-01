package stremio

import (
	"testing"
	"time"
)

func TestParseMediaInfo(t *testing.T) {
	cases := []struct {
		in          string
		wantType    string
		wantName    string
		wantDisplay string
	}{
		{"Breaking.Bad.S05E14.1080p.WEB-DL.x264", "series", "Breaking Bad", "Breaking Bad · S05E14"},
		{"Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1.H.265-FLUX.mkv", "movie", "Dune Part Two", "Dune Part Two (2024)"},
		{"[SubsPlease] Frieren - 60 [1080p].mkv", "series", "Frieren", "Frieren · E60"},
		{"The.Office.S02E03.Office.Olympics.HDTV.x264-EZTV.avi", "series", "The Office", "The Office · S02E03 – Office Olympics"},
	}
	for _, c := range cases {
		got := ParseMediaInfo(c.in)
		if got == nil {
			t.Fatalf("ParseMediaInfo(%q) = nil", c.in)
		}
		if got.Type != c.wantType || got.Name != c.wantName || got.Display != c.wantDisplay {
			t.Errorf("ParseMediaInfo(%q) = %+v, want type=%q name=%q display=%q",
				c.in, got, c.wantType, c.wantName, c.wantDisplay)
		}
	}
}

func TestStaleTorrentFiltered(t *testing.T) {
	// No streaming flag, no queue, no peers, started long ago, no cache
	// events: a leftover entry after the player closed.
	old := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	top := rankedTorrent{
		t: torrent{
			Name:    "Some.Movie.2023.1080p.WEB-DL.x264",
			Sources: []statsSource{{LastStarted: old}},
		},
	}
	if got := statusFromRanked(top); got.Media != nil {
		t.Errorf("stale torrent: expected nil media (browsing), got %+v", got.Media)
	}
}

func TestStreamingTorrentIsLive(t *testing.T) {
	top := rankedTorrent{
		t: torrent{
			Name:           "Dune.Part.Two.2024.1080p.WEB-DL.x264",
			Streaming:      true,
			StreamLen:      100,
			StreamProgress: 42,
			Sources:        []statsSource{{LastStarted: time.Now().UTC().Format(time.RFC3339)}},
		},
		latestStart: time.Now().UnixMilli(),
		streaming:   1,
	}
	got := statusFromRanked(top)
	if got.Media == nil {
		t.Fatal("streaming torrent: expected media, got nil")
	}
	if !got.HasProgress || got.Progress != 42 || got.Length != 100 {
		t.Errorf("progress not exposed: %+v", got)
	}
}
