// Package stremio detects the Stremio Desktop process and the currently
// streaming title via Stremio's local server (127.0.0.1:11470/stats.json).
// Ported from src/stremioDetector.js — only the behavior needed for presence.
package stremio

import (
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// MediaInfo is the structured result of ParseMediaInfo.
type MediaInfo struct {
	Type            string // "series" or "movie"
	Name            string
	Year            string
	Season          int
	Episode         int
	EpisodeTitle    string
	AbsoluteEpisode bool // anime absolute numbering: show E60, not S01E60
	SearchName      string
	Display         string
}

// IsStremioRunning reports whether the Stremio Desktop process is alive.
func IsStremioRunning() bool {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/NH").Output()
		if err != nil {
			return false
		}
		lower := strings.ToLower(string(out))
		return strings.Contains(lower, "stremio-shell-ng.exe") ||
			strings.Contains(lower, "stremio.exe")
	}
	// macOS / Linux fallback: pgrep like the original.
	for _, name := range []string{"stremio", "Stremio", "stremio-runtime"} {
		if err := exec.Command("pgrep", "-x", name).Run(); err == nil {
			return true
		}
	}
	return false
}

// ---- local stats.json plumbing ----

type statsSource struct {
	LastStarted string `json:"lastStarted"`
}

type torrentFile struct {
	Name   string          `json:"name"`
	Length int64           `json:"length"`
	Cache  json.RawMessage `json:"__cacheEvents"`
}

type torrent struct {
	Name             string        `json:"name"`
	Streaming        bool          `json:"streaming"`
	StreamLen        float64       `json:"streamLen"`
	StreamProgress   float64       `json:"streamProgress"`
	Queued           int           `json:"queued"`
	Unique           int           `json:"unique"`
	SwarmConnections int           `json:"swarmConnections"`
	Sources          []statsSource `json:"sources"`
	Files            []torrentFile `json:"files"`
}

type rankedTorrent struct {
	t           torrent
	streaming   int
	queued      int
	unique      int
	conns       int
	latestStart int64
}

var videoExtRe = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|mov|m4v|webm|ts|flv|wmv|mpg|mpeg)$`)

func cacheScore(f torrentFile) int {
	if len(f.Cache) == 0 || string(f.Cache) == "null" {
		if f.Length > 0 && f.Length < 1<<30 {
			return int(f.Length)
		}
		return 0
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(f.Cache, &arr); err == nil {
		return len(arr)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(f.Cache, &obj); err == nil {
		return len(obj)
	}
	return 1
}

func hasCacheEvents(f torrentFile) bool {
	s := strings.TrimSpace(string(f.Cache))
	return s != "" && s != "null"
}

func pickLargestVideoFile(files []torrentFile) *torrentFile {
	if len(files) == 0 {
		return nil
	}
	var pool []torrentFile
	for _, f := range files {
		if f.Name != "" && videoExtRe.MatchString(f.Name) {
			pool = append(pool, f)
		}
	}
	if len(pool) == 0 {
		pool = files
	}
	best := pool[0]
	for _, f := range pool[1:] {
		if f.Length > best.Length {
			best = f
		}
	}
	out := best
	return &out
}

// StreamStatus is the live view of the active stream: parsed title plus the
// raw signals used for play/pause inference and stale-torrent filtering.
type StreamStatus struct {
	Media       *MediaInfo
	HasProgress bool
	Progress    float64
	Length      float64
	// CacheScore grows while the player reads the file; stalls when paused.
	CacheScore int
}

// FetchActiveStream queries 127.0.0.1:11470/stats.json and returns the active
// stream, or a zero StreamStatus when nothing is streaming (browsing), the
// server is unreachable, or the top torrent looks stale (player closed but
// the entry lingers in stats).
func FetchActiveStream() StreamStatus {
	client := &http.Client{Timeout: 1200 * time.Millisecond}
	resp, err := client.Get("http://127.0.0.1:11470/stats.json")
	if err != nil {
		return StreamStatus{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return StreamStatus{}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return StreamStatus{}
	}
	var stats map[string]torrent
	if err := json.Unmarshal(body, &stats); err != nil {
		return StreamStatus{}
	}
	if len(stats) == 0 {
		return StreamStatus{}
	}

	var list []rankedTorrent
	for _, t := range stats {
		var latest int64
		for _, s := range t.Sources {
			if s.LastStarted == "" {
				continue
			}
			if tm, err := time.Parse(time.RFC3339, s.LastStarted); err == nil {
				if ms := tm.UnixMilli(); ms > latest {
					latest = ms
				}
			}
		}
		s := 0
		if t.Streaming || t.StreamLen > 0 || t.StreamProgress > 0 {
			s = 1
		}
		list = append(list, rankedTorrent{t, s, t.Queued, t.Unique, t.SwarmConnections, latest})
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.streaming != b.streaming {
			return a.streaming > b.streaming
		}
		if a.queued != b.queued {
			return a.queued > b.queued
		}
		if a.unique != b.unique {
			return a.unique > b.unique
		}
		if a.conns != b.conns {
			return a.conns > b.conns
		}
		return a.latestStart > b.latestStart
	})
	return statusFromRanked(list[0])
}

// statusFromRanked builds a StreamStatus from the top-ranked torrent.
// Split out for testability (no HTTP needed).
func statusFromRanked(top rankedTorrent) StreamStatus {
	active := top.t

	var rawTitle string
	var activeFile *torrentFile
	activeHasCache := false
	if len(active.Files) > 0 {
		var cached []torrentFile
		for _, f := range active.Files {
			if hasCacheEvents(f) {
				cached = append(cached, f)
			}
		}
		switch {
		case len(cached) == 1:
			c := cached[0]
			activeFile = &c
		case len(cached) > 1:
			best := cached[0]
			for _, f := range cached[1:] {
				if cacheScore(f) > cacheScore(best) {
					best = f
				}
			}
			c := best
			activeFile = &c
		default:
			activeFile = pickLargestVideoFile(active.Files)
		}
		if activeFile != nil && activeFile.Name != "" {
			rawTitle = activeFile.Name
			activeHasCache = hasCacheEvents(*activeFile)
		} else {
			rawTitle = active.Name
		}
	} else {
		rawTitle = active.Name
	}
	if strings.TrimSpace(rawTitle) == "" {
		return StreamStatus{}
	}
	fresh := top.latestStart > 0 && time.Since(time.UnixMilli(top.latestStart)) < 10*time.Minute
	live := top.streaming == 1 || top.queued > 0 || top.conns > 0 || fresh || activeHasCache
	if !live {
		// Stale torrent lingering in stats after the player closed:
		// report browsing instead of a stuck title.
		return StreamStatus{}
	}
	media := ParseMediaInfo(rawTitle)
	return StreamStatus{
		Media:       media,
		HasProgress: active.StreamLen > 0 || active.StreamProgress > 0,
		Progress:    active.StreamProgress,
		Length:      active.StreamLen,
		CacheScore:  cacheScoreOf(activeFile),
	}
}

// FetchActiveMedia is FetchActiveStream returning just the parsed title
// (nil when browsing). Kept for compatibility.
func FetchActiveMedia() *MediaInfo {
	return FetchActiveStream().Media
}

func cacheScoreOf(f *torrentFile) int {
	if f == nil {
		return 0
	}
	return cacheScore(*f)
}

// ---- title parsing (ported from stremioDetector.js) ----

var (
	extRe          = regexp.MustCompile(`\.[a-zA-Z0-9]{2,4}$`)
	bracketRe      = regexp.MustCompile(`[\[(][^\][()]*[\])]`)
	yearRe         = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	yearParenRe    = regexp.MustCompile(`\(?\b(19|20)\d{2}\b\)?`)
	seRe           = regexp.MustCompile(`(?i)\bS(\d{1,2})\s*E(\d{1,3})\b`)
	xRe            = regexp.MustCompile(`\b(\d{1,2})x(\d{1,3})\b`)
	seSearchRe     = regexp.MustCompile(`(?i)\bS\d{1,2}\s*E\d{1,3}\b|\b\d{1,2}x\d{1,3}\b`)
	groupTagRe     = regexp.MustCompile(`^\s*[\[(][^\][()]*[\])]`)
	emptyParenRe   = regexp.MustCompile(`\(\s*\)`)
	emptyBracketRe = regexp.MustCompile(`\[\s*\]`)
	trailGroupRe   = regexp.MustCompile(`\s+-\s*([A-Za-z0-9]{2,})\s*$`)
	leadSepRe      = regexp.MustCompile(`^[([{<]+\s*$`)
	trailLeadRe    = regexp.MustCompile(`^\s*[)\]}>]+`)
	trailDigitRe   = regexp.MustCompile(`\s+\d\s*$`)
	multiSpaceRe   = regexp.MustCompile(`\s+`)
	edgeDashRe     = regexp.MustCompile(`^[-–·]\s*|\s*[-–·]\s*$`)
	edgeDashSpRe   = regexp.MustCompile(`^[-–·\s]+|[-–·\s]+$`)
)

var tagRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b\d{3,4}p\b`),
	regexp.MustCompile(`(?i)\b2160p\b`),
	regexp.MustCompile(`(?i)\bx26[45]\b`),
	regexp.MustCompile(`(?i)\bh[\s.]?26[45]\b`),
	regexp.MustCompile(`(?i)\bhevc\b`),
	regexp.MustCompile(`(?i)\bavc\b`),
	regexp.MustCompile(`(?i)\bweb[\s-]?dl\b`),
	regexp.MustCompile(`(?i)\bweb[\s-]?rip\b`),
	regexp.MustCompile(`(?i)\bweb\b`),
	regexp.MustCompile(`(?i)\bhdtv\b`),
	regexp.MustCompile(`(?i)\bbluray\b`),
	regexp.MustCompile(`(?i)\bblu[\s-]?ray\b`),
	regexp.MustCompile(`(?i)\bbrrip\b`),
	regexp.MustCompile(`(?i)\bbdrip\b`),
	regexp.MustCompile(`(?i)\bdvdrip\b`),
	regexp.MustCompile(`(?i)\bhdr10?\b`),
	regexp.MustCompile(`(?i)\bhdr\b`),
	regexp.MustCompile(`(?i)\bsdr\b`),
	regexp.MustCompile(`(?i)\bdolby\b`),
	regexp.MustCompile(`(?i)\batmos\b`),
	regexp.MustCompile(`(?i)\bmulti\b`),
	regexp.MustCompile(`(?i)\bdual\b`),
	regexp.MustCompile(`(?i)\bdual[\s-]?audio\b`),
	regexp.MustCompile(`(?i)\bsubbed\b`),
	regexp.MustCompile(`(?i)\bdubbed\b`),
	regexp.MustCompile(`(?i)\besub\b`),
	regexp.MustCompile(`(?i)\bdts\b`),
	regexp.MustCompile(`(?i)\bddp?5[\s.]?1\b`),
	regexp.MustCompile(`(?i)\bddp?7[\s.]?1\b`),
	regexp.MustCompile(`(?i)\bddp?5\b`),
	regexp.MustCompile(`(?i)\bddp\b`),
	regexp.MustCompile(`(?i)\bdd\b`),
	regexp.MustCompile(`(?i)\beac3\b`),
	regexp.MustCompile(`(?i)\bac3\b`),
	regexp.MustCompile(`(?i)\baac\b`),
	regexp.MustCompile(`\b\d\.[01]\b`),
	regexp.MustCompile(`(?i)\b10bit\b`),
	regexp.MustCompile(`(?i)\b8bit\b`),
	regexp.MustCompile(`(?i)\bdv\b`),
	regexp.MustCompile(`(?i)\bamzn\b`),
	regexp.MustCompile(`(?i)\bnf\b`),
	regexp.MustCompile(`(?i)\bdsnp\b`),
	regexp.MustCompile(`(?i)\bhmax\b`),
	regexp.MustCompile(`(?i)\bhulu\b`),
	regexp.MustCompile(`(?i)\batvp\b`),
	regexp.MustCompile(`(?i)\bpcok\b`),
	regexp.MustCompile(`(?i)\bpmtp\b`),
	regexp.MustCompile(`(?i)\bime\b`),
	regexp.MustCompile(`(?i)\bhiggsboson\b`),
	regexp.MustCompile(`(?i)\bmegusta\b`),
	regexp.MustCompile(`(?i)\bwww\b`),
	regexp.MustCompile(`(?i)\buindex\b`),
	regexp.MustCompile(`(?i)\borg\b`),
	regexp.MustCompile(`(?i)\byify\b`),
	regexp.MustCompile(`(?i)\byts\b`),
	regexp.MustCompile(`(?i)\bbrarbg\b`),
	regexp.MustCompile(`(?i)\beztvx?\b`),
	regexp.MustCompile(`(?i)\bethel\b`),
	regexp.MustCompile(`(?i)\bntb\b`),
	regexp.MustCompile(`(?i)\bflux\b`),
	regexp.MustCompile(`(?i)\bsuccessfulcrab\b`),
	regexp.MustCompile(`(?i)\bggwp\b`),
	regexp.MustCompile(`(?i)\bggez\b`),
	regexp.MustCompile(`(?i)\bedith\b`),
	regexp.MustCompile(`(?i)\bnhtfs\b`),
	regexp.MustCompile(`(?i)\bcakes\b`),
	regexp.MustCompile(`(?i)\bsubsplease\b`),
	regexp.MustCompile(`(?i)\bhorriblesubs\b`),
	regexp.MustCompile(`(?i)\berai[\s-]?raws\b`),
	regexp.MustCompile(`(?i)\basw\b`),
	regexp.MustCompile(`(?i)\bremastered\b`),
	regexp.MustCompile(`(?i)\bproper\b`),
	regexp.MustCompile(`(?i)\brepack\b`),
	regexp.MustCompile(`(?i)\bextended\b`),
	regexp.MustCompile(`(?i)\bunrated\b`),
	regexp.MustCompile(`(?i)\bt3nzin\b`),
	regexp.MustCompile(`(?i)\bgalaxytv\b`),
	regexp.MustCompile(`(?i)\btgx\b`),
}

func cleanTitle(title string) string {
	if title == "" {
		return ""
	}
	cleaned := extRe.ReplaceAllString(title, "")
	cleaned = bracketRe.ReplaceAllString(cleaned, " ")
	cleaned = strings.Map(func(r rune) rune {
		if r == '.' || r == '_' {
			return ' '
		}
		return r
	}, cleaned)
	for _, re := range tagRes {
		cleaned = re.ReplaceAllString(cleaned, "")
	}
	cleaned = emptyParenRe.ReplaceAllString(cleaned, "")
	cleaned = emptyBracketRe.ReplaceAllString(cleaned, "")
	// trailing release group like "-RARBG" (keep "Spider-Man")
	cleaned = trailGroupRe.ReplaceAllStringFunc(cleaned, func(m string) string {
		sub := trailGroupRe.FindStringSubmatch(m)
		if len(sub) != 2 {
			return m
		}
		grp := sub[1]
		isAllCaps := grp == strings.ToUpper(grp) && strings.ContainsAny(grp, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
		hasUpper := strings.ContainsAny(grp, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
		hasLower := strings.ContainsAny(grp, "abcdefghijklmnopqrstuvwxyz")
		if isAllCaps || (len(grp) <= 6 && hasUpper && hasLower) {
			return " "
		}
		return m
	})
	// NOTE: leadSepRe / trailLeadRe operate on the whole string in JS after
	// tag removal; that only strips when the entire remainder is separators.
	cleaned = leadSepRe.ReplaceAllString(cleaned, "")
	cleaned = trailLeadRe.ReplaceAllString(cleaned, "")
	cleaned = trailDigitRe.ReplaceAllString(cleaned, "")
	cleaned = multiSpaceRe.ReplaceAllString(cleaned, " ")
	cleaned = strings.TrimSpace(cleaned)
	cleaned = edgeDashRe.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func stripYear(s string) string {
	if s == "" {
		return s
	}
	out := yearParenRe.ReplaceAllString(s, " ")
	out = emptyParenRe.ReplaceAllString(out, "")
	out = emptyBracketRe.ReplaceAllString(out, "")
	out = multiSpaceRe.ReplaceAllString(out, " ")
	out = strings.TrimSpace(out)
	out = edgeDashSpRe.ReplaceAllString(out, "")
	return strings.TrimSpace(out)
}

func sanitizeEpisodeTitle(s string) string {
	t := strings.TrimSpace(s)
	if len(t) < 2 {
		return ""
	}
	hasLetter := false
	for _, r := range t {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			hasLetter = true
			break
		}
	}
	if !hasLetter {
		return ""
	}
	if !strings.Contains(t, " ") && t == strings.ToLower(t) {
		return ""
	}
	if !strings.Contains(t, " ") && t == strings.ToUpper(t) && len(t) <= 8 {
		return ""
	}
	return t
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// ParseMediaInfo turns a raw torrent/file name into structured media info.
func ParseMediaInfo(rawName string) *MediaInfo {
	if strings.TrimSpace(rawName) == "" {
		return nil
	}
	base := extRe.ReplaceAllString(rawName, "")
	base = strings.Map(func(r rune) rune {
		if r == '.' || r == '_' {
			return ' '
		}
		return r
	}, base)

	year := ""
	if m := yearRe.FindString(base); m != "" {
		year = m
	}

	if m := seRe.FindStringSubmatch(base); m != nil {
		season, episode := atoi(m[1]), atoi(m[2])
		idx := seSearchRe.FindStringIndex(base)
		before, after := base, ""
		if idx != nil {
			if idx[0] > 0 {
				before = base[:idx[0]]
			} else {
				before = base
			}
			after = base[idx[1]:]
		}
		name := stripYear(cleanTitle(before))
		if name == "" {
			if c := cleanTitle(base); c != "" {
				name = c
			} else {
				name = "Unknown"
			}
		}
		epTitle := sanitizeEpisodeTitle(stripYear(cleanTitle(after)))
		epLabel := "S" + pad2(season) + "E" + pad2(episode)
		display := name + " · " + epLabel
		if epTitle != "" {
			display += " – " + epTitle
		}
		return &MediaInfo{Type: "series", Name: name, Year: year, Season: season,
			Episode: episode, EpisodeTitle: epTitle, SearchName: name, Display: display}
	}
	if m := xRe.FindStringSubmatch(base); m != nil {
		// 1x09 form — guard: only when it really looks like S/E, i.e. the
		// prefix before it is non-empty after cleaning.
		idx := seSearchRe.FindStringIndex(base)
		before, after := base, ""
		if idx != nil {
			if idx[0] > 0 {
				before = base[:idx[0]]
			}
			after = base[idx[1]:]
		}
		season, episode := atoi(m[1]), atoi(m[2])
		name := stripYear(cleanTitle(before))
		if name != "" {
			epTitle := sanitizeEpisodeTitle(stripYear(cleanTitle(after)))
			epLabel := "S" + pad2(season) + "E" + pad2(episode)
			display := name + " · " + epLabel
			if epTitle != "" {
				display += " – " + epTitle
			}
			return &MediaInfo{Type: "series", Name: name, Year: year, Season: season,
				Episode: episode, EpisodeTitle: epTitle, SearchName: name, Display: display}
		}
	}

	// Anime absolute episode: "[Group] Show - 60".
	if year == "" {
		hasGroupTag := groupTagRe.MatchString(base)
		noBrackets := multiSpaceRe.ReplaceAllString(bracketRe.ReplaceAllString(base, " "), " ")
		noBrackets = strings.TrimSpace(noBrackets)
		var re *regexp.Regexp
		if hasGroupTag {
			re = regexp.MustCompile(`^(.*?)\s+-?\s*(\d{1,4})\s*$`)
		} else {
			re = regexp.MustCompile(`^(.*?)\s+-\s*(\d{1,4})\s*$`)
		}
		if m := re.FindStringSubmatch(noBrackets); m != nil && strings.TrimSpace(m[1]) != "" {
			animeName := stripYear(cleanTitle(m[1]))
			if animeName != "" && strings.ContainsAny(animeName, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
				ep := atoi(m[2])
				return &MediaInfo{Type: "series", Name: animeName,
					Episode: ep, AbsoluteEpisode: true, SearchName: animeName,
					Display: animeName + " · E" + pad2(ep)}
			}
		}
	}

	name := stripYear(cleanTitle(base))
	if name == "" {
		name = "Unknown"
	}
	display := name
	if year != "" {
		display = name + " (" + year + ")"
	}
	return &MediaInfo{Type: "movie", Name: name, Year: year, SearchName: name, Display: display}
}
