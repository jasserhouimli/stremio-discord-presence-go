package updater

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.1.0", "1.0.0", 1},
		{"1.0.0", "1.1.0", -1},
		{"1.0.0", "1.0.0", 0},
		{"v1.2.0", "1.1.9", 1},     // leading v ignored
		{"1.2", "1.2.0", 0},        // trailing zero equivalent
		{"1.2.1", "1.2", 1},        // non-zero tail is newer
		{"1.0.0", "1.0.0-beta", 1}, // release beats prerelease
		{"", "1.0.0", -1},          // unknown/dev counts as older
		{"dev", "0.0.1", -1},       // dev builds still update
		{"1.10.0", "1.9.9", 1},     // numeric, not lexical
		{"2.0", "10.0", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestPickAssetPrefersGoBuild(t *testing.T) {
	rel := &ghRelease{Tag: "v1.1.0", Assets: []ghAsset{
		{Name: "Stremio-Discord-Presence-Setup-1.1.0.exe", URL: "https://example.com/setup"},
		{Name: "stremio-discord-presence-go-1.1.0.exe", URL: "https://example.com/go"},
	}}
	got := pickAsset(rel, "stremio-discord-presence.exe")
	if got == nil || got.URL != "https://example.com/go" {
		t.Fatalf("expected the Go exe asset, got %+v", got)
	}
}

func TestPickAssetFallsBackToFirstExe(t *testing.T) {
	rel := &ghRelease{Tag: "v1.1.0", Assets: []ghAsset{
		{Name: "notes.txt", URL: "https://example.com/notes"},
		{Name: "Something-1.1.0.exe", URL: "https://example.com/a"},
	}}
	got := pickAsset(rel, "stremio-discord-presence.exe")
	if got == nil || got.URL != "https://example.com/a" {
		t.Fatalf("expected fallback exe, got %+v", got)
	}
}

func TestPickAssetNoneWhenNoExe(t *testing.T) {
	rel := &ghRelease{Tag: "v1.1.0", Assets: []ghAsset{
		{Name: "notes.txt", URL: "https://example.com/notes"},
	}}
	if got := pickAsset(rel, "x.exe"); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}
