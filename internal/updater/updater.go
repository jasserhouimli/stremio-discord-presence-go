// Package updater implements self-updates from GitHub Releases without any
// Electron dependency: check the latest release, download the exe asset with
// progress, then swap it over the running binary via a helper batch file and
// relaunch. The caller (tray) owns all UI through Events.
package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Events reports update lifecycle to the UI. All are optional.
type Events struct {
	OnChecking  func(manual bool)
	OnAvailable func(version string)
	OnProgress  func(downloaded, total int64)
	OnReady     func(version string)
	OnUpToDate  func()
	OnError     func(msg string)
}

// Updater checks a GitHub repo's latest release for a newer exe.
type Updater struct {
	// Current is this build's version, e.g. "1.0.0" (tags compared sans "v").
	Current string
	// Repo is "owner/name", e.g. "MacroMaster101/Stremio_Discord_Rich_Presence".
	Repo string
	// ExeName is this binary's file name, used to prefer the right asset.
	ExeName string
	Events  Events

	mu           sync.Mutex
	checking     bool
	readyVersion string
	readyPath    string
	apiTimeout   time.Duration
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type ghRelease struct {
	Tag    string    `json:"tag_name"`
	Assets []ghAsset `json:"assets"`
}

func (u *Updater) timeout() time.Duration {
	if u.apiTimeout > 0 {
		return u.apiTimeout
	}
	return 15 * time.Second
}

// Ready returns the downloaded version/path when an update awaits install.
func (u *Updater) Ready() (version, path string, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.readyVersion == "" || u.readyPath == "" {
		return "", "", false
	}
	return u.readyVersion, u.readyPath, true
}

// Check looks for a newer release (async). Manual checks report every
// outcome; silent ones only surface available/ready.
func (u *Updater) Check(manual bool) {
	u.mu.Lock()
	if u.checking {
		u.mu.Unlock()
		return
	}
	u.checking = true
	u.mu.Unlock()

	go func() {
		defer func() {
			u.mu.Lock()
			u.checking = false
			u.mu.Unlock()
		}()
		u.emitChecking(manual)
		rel, err := u.latestRelease()
		if err != nil {
			// Silent checks must not leave the menu stuck on "Checking…".
			if manual {
				u.emitError("Could not check for updates. Try again later.")
			}
			return
		}
		latest := strings.TrimPrefix(strings.TrimSpace(rel.Tag), "v")
		if latest == "" || compareVersions(latest, u.Current) <= 0 {
			if manual {
				u.emitUpToDate()
			}
			return
		}
		u.emitAvailable(latest)
		path, err := u.download(rel, latest, manual)
		if err != nil {
			if manual {
				u.emitError(err.Error())
			}
			return
		}
		u.mu.Lock()
		u.readyVersion, u.readyPath = latest, path
		u.mu.Unlock()
		u.emitReady(latest)
	}()
}

func (u *Updater) latestRelease() (*ghRelease, error) {
	client := &http.Client{Timeout: u.timeout()}
	url := "https://api.github.com/repos/" + u.Repo + "/releases/latest"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "stremio-discord-presence-go-updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github api: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// pickAsset chooses the exe to install: exact exe-name match first, then a
// name mentioning "go" (Go builds next to old Electron Setup installers),
// then any stremio exe, then the first exe as a last resort.
func pickAsset(rel *ghRelease, exeName string) *ghAsset {
	var firstExe, stremioExe, goExe *ghAsset
	for i := range rel.Assets {
		a := &rel.Assets[i]
		if !strings.HasSuffix(strings.ToLower(a.Name), ".exe") {
			continue
		}
		if firstExe == nil {
			firstExe = a
		}
		lower := strings.ToLower(a.Name)
		if exeName != "" && strings.EqualFold(a.Name, exeName) {
			return a
		}
		if strings.Contains(lower, "stremio") {
			if stremioExe == nil {
				stremioExe = a
			}
			if strings.Contains(lower, "go") {
				if goExe == nil {
					goExe = a
				}
			}
		}
	}
	if goExe != nil {
		return goExe
	}
	if stremioExe != nil {
		return stremioExe
	}
	return firstExe
}

func (u *Updater) download(rel *ghRelease, version string, manual bool) (string, error) {
	asset := pickAsset(rel, u.ExeName)
	if asset == nil || asset.URL == "" {
		return "", fmt.Errorf("no installer found in the latest release")
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequest("GET", asset.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "stremio-discord-presence-go-updater")
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}

	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("sdp-update-%s.exe", version))
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	var written int64
	total := resp.ContentLength
	buf := make([]byte, 128*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(tmp)
				return "", werr
			}
			written += int64(n)
			u.emitProgress(written, total)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			f.Close()
			os.Remove(tmp)
			return "", rerr
		}
	}
	f.Close()

	if err := verifyExe(tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	_ = manual
	return tmp, nil
}

// verifyExe is a cheap sanity check: the payload must look like a Windows
// executable and not be trivially small (an error page, a stub, ...).
func verifyExe(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Size() < 1<<20 {
		return fmt.Errorf("downloaded file looks too small to be the app")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 2)
	if _, err := io.ReadFull(f, magic); err != nil {
		return fmt.Errorf("downloaded file is unreadable")
	}
	if magic[0] != 'M' || magic[1] != 'Z' {
		return fmt.Errorf("downloaded file is not a Windows executable")
	}
	return nil
}

// ApplyAndRestart swaps the downloaded exe over the running one and
// relaunches. Windows cannot replace a running binary, so a helper batch
// file waits for this process to exit, moves the file, restarts, and deletes
// itself. The caller must quit immediately after a nil return.
func (u *Updater) ApplyAndRestart() error {
	u.mu.Lock()
	path := u.readyPath
	u.mu.Unlock()
	if path == "" {
		return fmt.Errorf("no downloaded update to install")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	bat := filepath.Join(os.TempDir(), "sdp-apply-update.bat")
	script := "@echo off\r\n" +
		"timeout /t 2 /nobreak >nul\r\n" +
		":wait\r\n" +
		"move /Y \"" + path + "\" \"" + exe + "\" >nul 2>&1\r\n" +
		"if errorlevel 1 ( timeout /t 1 /nobreak >nul & goto wait )\r\n" +
		"start \"\" \"" + exe + "\"\r\n" +
		"del \"%~f0\"\r\n"
	if err := os.WriteFile(bat, []byte(script), 0600); err != nil {
		return err
	}
	// start /min avoids a visible console flash.
	cmd := exec.Command("cmd", "/C", "start", "/min", "", bat)
	cmd.Dir = os.TempDir()
	if err := cmd.Start(); err != nil {
		os.Remove(bat)
		return err
	}
	return nil
}

// compareVersions compares dotted versions ("1.2.3", optional leading "v",
// optional "-suffix"). Returns -1/0/+1. Unknown/empty counts as older than
// any real version so dev builds still update.
func compareVersions(a, b string) int {
	ca, cb := parseVersion(a), parseVersion(b)
	if len(ca.nums) == 0 && len(cb.nums) == 0 {
		return 0
	}
	if len(ca.nums) == 0 {
		return -1
	}
	if len(cb.nums) == 0 {
		return 1
	}
	for i := 0; i < len(ca.nums) && i < len(cb.nums); i++ {
		if ca.nums[i] != cb.nums[i] {
			if ca.nums[i] < cb.nums[i] {
				return -1
			}
			return 1
		}
	}
	if len(ca.nums) != len(cb.nums) {
		// 1.2 vs 1.2.0: longer is newer only if the tail is non-zero.
		longer, _ := ca, cb
		if len(cb.nums) > len(ca.nums) {
			longer = cb
		}
		for _, n := range longer.nums[min(len(ca.nums), len(cb.nums)):] {
			if n != 0 {
				if len(ca.nums) > len(cb.nums) {
					return 1
				}
				return -1
			}
		}
	}
	// Same core: a bare release beats a suffixed prerelease ("1.0" > "1.0-beta").
	if ca.suffix != cb.suffix {
		if ca.suffix == "" {
			return 1
		}
		if cb.suffix == "" {
			return -1
		}
		return strings.Compare(ca.suffix, cb.suffix)
	}
	return 0
}

type parsedVersion struct {
	nums   []int
	suffix string
}

func parseVersion(s string) parsedVersion {
	s = strings.TrimSpace(strings.TrimPrefix(s, "v"))
	core := s
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		core = s[:i]
	}
	var out parsedVersion
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		out.suffix = s[i+1:]
	}
	for _, part := range strings.Split(core, ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			// Non-numeric core (e.g. "dev"): unknown version.
			return parsedVersion{}
		}
		out.nums = append(out.nums, n)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (u *Updater) emitChecking(manual bool) {
	if u.Events.OnChecking != nil {
		u.Events.OnChecking(manual)
	}
}

func (u *Updater) emitAvailable(v string) {
	if u.Events.OnAvailable != nil {
		u.Events.OnAvailable(v)
	}
}

func (u *Updater) emitProgress(downloaded, total int64) {
	if u.Events.OnProgress != nil {
		u.Events.OnProgress(downloaded, total)
	}
}

func (u *Updater) emitReady(v string) {
	if u.Events.OnReady != nil {
		u.Events.OnReady(v)
	}
}

func (u *Updater) emitUpToDate() {
	if u.Events.OnUpToDate != nil {
		u.Events.OnUpToDate()
	}
}

func (u *Updater) emitError(msg string) {
	if u.Events.OnError != nil {
		u.Events.OnError(msg)
	}
}
