// Stremio Discord Presence (Go) — lightweight tray companion.
//
// Core behavior only (ported from the Electron app):
//   - detect Stremio Desktop (tasklist) + active stream (127.0.0.1:11470)
//   - parse torrent/file names into series/movie titles
//   - optional poster art via Stremio's public Cinemeta addon
//   - Discord Rich Presence over local IPC, with 15s reconnect
//   - tray menu: status, Privacy Mode, poster toggle, reconnect, quit
//
// Deliberately omitted: auto-updater, start-with-Windows, About window,
// theme-aware icons. That is what keeps the binary at ~10MB vs ~200MB.
package main

import (
	"bufio"
	_ "embed"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/getlantern/systray"
	"github.com/macromaster101/stremio-discord-presence-go/internal/cinemeta"
	"github.com/macromaster101/stremio-discord-presence-go/internal/presence"
	"github.com/macromaster101/stremio-discord-presence-go/internal/startup"
	"github.com/macromaster101/stremio-discord-presence-go/internal/stremio"
	"github.com/macromaster101/stremio-discord-presence-go/internal/updater"
)

//go:embed icon.ico
var appIcon []byte

const (
	clientID          = "1511758462089035988" // built-in Discord app (public ID, not a secret)
	appVersion        = "1.0.0"               // bump per release; compared against GitHub tag (sans "v")
	defaultUpdateRepo = "MacroMaster101/Stremio_Discord_Rich_Presence"
	defaultPollMs     = 5000
	defaultActiveMs   = 2500
	defaultPauseAfter = 0                 // 0 = off: never guess Paused (see shouldMarkPaused)
	defaultStopGrace  = 120               // seconds to show "Stopped" after a stream ends
	singleInstanceTCP = "127.0.0.1:47923" // held for app lifetime; 2nd copy exits
)

// ---- tiny .env loader (no dependency) ----

func loadDotEnv() {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".env"))
	}
	candidates = append(candidates, ".env")
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			applyDotEnv(p)
			return
		}
	}
}

func applyDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(strings.Trim(kv[1], `"' `))
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

// ---- app state ----

var (
	pollInterval    = defaultPollMs
	activePollMs    = defaultActiveMs
	pauseAfterSec   = defaultPauseAfter
	stopGraceSec    = defaultStopGrace
	privacyMode     = false
	showPosters     = true
	updateRepo      = defaultUpdateRepo
	updateSilent    = true
	rpc             *presence.Manager
	up              *updater.Updater
	updatingTo      string
	installLaunched bool
	discordStatus   = "Disconnected"
	stremioRun      = false

	mDiscord *systray.MenuItem
	mStremio *systray.MenuItem
	mPrivacy *systray.MenuItem
	mPosters *systray.MenuItem
	mStartup *systray.MenuItem
	mCheck   *systray.MenuItem
	mRestart *systray.MenuItem
	mUpdate  *systray.MenuItem
)

func envString(name, def string) string {
	if s := strings.TrimSpace(os.Getenv(name)); s != "" {
		return s
	}
	return def
}

func envInt(name string, def, min int) int {
	if s := strings.TrimSpace(os.Getenv(name)); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= min {
			return n
		}
	}
	return def
}

func envPrivacy() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("PRIVACY_MODE")), "true")
}

func main() {
	loadDotEnv()
	pollInterval = envInt("POLL_INTERVAL_MS", defaultPollMs, 1000)
	activePollMs = envInt("ACTIVE_POLL_INTERVAL_MS", defaultActiveMs, 1000)
	pauseAfterSec = envInt("PAUSE_AFTER_SECONDS", defaultPauseAfter, 0)
	stopGraceSec = envInt("STOP_GRACE_SECONDS", defaultStopGrace, 0)
	privacyMode = envPrivacy()
	updateRepo = envString("UPDATE_REPO", defaultUpdateRepo)
	updateSilent = !strings.EqualFold(strings.TrimSpace(os.Getenv("UPDATE_CHECK")), "false")
	rpc = presence.New(clientID)
	up = newUpdater()

	// Clean up Run entries left by older (Electron) builds; adopt them if on.
	startup.MigrateLegacyEntries()

	// Single instance: hold a loopback port; exit if it's taken.
	ln, err := net.Listen("tcp", singleInstanceTCP)
	if err != nil {
		fmt.Println("Another instance is already running. Exiting...")
		os.Exit(0)
	}
	defer ln.Close()

	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(appIcon)
	systray.SetTitle("Stremio Discord Presence")
	systray.SetTooltip("Stremio Discord Presence")

	mDiscord = systray.AddMenuItem("Discord: Disconnected", "Discord connection status")
	mDiscord.Disable()
	mStremio = systray.AddMenuItem("Stremio: ...", "Stremio detection status")
	mStremio.Disable()
	systray.AddSeparator()

	privacyLabel := "Privacy Mode: Off"
	if privacyMode {
		privacyLabel = "Privacy Mode: On"
	}
	mPrivacy = systray.AddMenuItemCheckbox(privacyLabel, "Hide what you are watching", privacyMode)
	if privacyMode {
		mPrivacy.Check()
	} else {
		mPrivacy.Uncheck()
	}
	mPosters = systray.AddMenuItemCheckbox("Show Poster Art: On", "Fetch posters from Cinemeta", true)
	mPosters.Check()
	mStartup = systray.AddMenuItemCheckbox("Start with Windows: Off", "Launch at login", false)
	refreshStartupItem()
	systray.AddSeparator()
	mReconnect := systray.AddMenuItem("Reconnect Discord", "Reconnect to Discord now")
	mCheck = systray.AddMenuItem("Check for Updates", "Check GitHub for a new version")
	mRestart = systray.AddMenuItem("Restart to update", "Install the downloaded update")
	mRestart.Hide()
	mUpdate = systray.AddMenuItem("Updates: idle", "Auto-update status")
	mUpdate.Disable()
	mQuit := systray.AddMenuItem("Quit", "Quit the app")

	up.Events = updater.Events{
		OnChecking: func(manual bool) {
			if manual && mUpdate != nil {
				mUpdate.SetTitle("Updates: checking…")
			}
		},
		OnAvailable: func(version string) {
			updatingTo = version
			if mUpdate != nil {
				mUpdate.SetTitle("Updates: downloading " + version + "… 0%")
			}
		},
		OnProgress: func(downloaded, total int64) {
			if mUpdate == nil || updatingTo == "" {
				return
			}
			if total > 0 {
				pct := downloaded * 100 / total
				mUpdate.SetTitle(fmt.Sprintf("Updates: downloading %s… %d%%", updatingTo, pct))
			} else {
				mUpdate.SetTitle(fmt.Sprintf("Updates: downloading %s… %.1f MB", updatingTo, float64(downloaded)/(1<<20)))
			}
		},
		OnReady: func(version string) {
			if mUpdate != nil {
				mUpdate.SetTitle("Updates: ready — " + version)
			}
			if mRestart != nil {
				mRestart.SetTitle("Restart to install " + version)
				mRestart.Show()
			}
		},
		OnUpToDate: func() {
			if mUpdate != nil {
				mUpdate.SetTitle("Updates: up to date")
			}
		},
		OnError: func(msg string) {
			if mUpdate != nil {
				mUpdate.SetTitle("Updates: failed — try again")
			}
			log.Println("Updater:", msg)
		},
	}

	rpc.OnStatus(func(status string) {
		discordStatus = status
		if mDiscord != nil {
			mDiscord.SetTitle("Discord: " + status)
		}
		// Push fresh presence right after (re)connect.
		if status == "Connected" {
			go runDetection(true)
		}
	})

	go func() {
		for {
			select {
			case <-mPrivacy.ClickedCh:
				privacyMode = !privacyMode
				if privacyMode {
					mPrivacy.Check()
					mPrivacy.SetTitle("Privacy Mode: On")
				} else {
					mPrivacy.Uncheck()
					mPrivacy.SetTitle("Privacy Mode: Off")
				}
				go runDetection(true)
			case <-mPosters.ClickedCh:
				showPosters = !showPosters
				if showPosters {
					mPosters.Check()
					mPosters.SetTitle("Show Poster Art: On")
				} else {
					mPosters.Uncheck()
					mPosters.SetTitle("Show Poster Art: Off")
				}
				go runDetection(true)
			case <-mStartup.ClickedCh:
				active := startup.SetEnabled(!startup.IsEnabled())
				log.Printf("Auto-start at login %s.", onOff(active))
				refreshStartupItem()
			case <-mReconnect.ClickedCh:
				log.Println("Manual Discord reconnect requested.")
				go rpc.Connect()
			case <-mCheck.ClickedCh:
				log.Println("Manual update check requested.")
				go up.Check(true)
			case <-mRestart.ClickedCh:
				go restartToUpdate()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()

	go rpc.Connect()
	go runDetection(true)
	// Silent update check shortly after startup (manual checks report all
	// outcomes; silent ones only surface available/ready).
	if updateSilent {
		go func() {
			time.Sleep(8 * time.Second)
			up.Check(false)
		}()
	}
	go func() {
		// Adaptive polling: fast while something is playing so
		// play/pause flips feel instant, slow while idle.
		for {
			interval := pollInterval
			if stremioRun && lastTitle != "" && !pausedNow {
				interval = activePollMs
			}
			time.Sleep(time.Duration(interval) * time.Millisecond)
			runDetection(false)
		}
	}()
}

func onExit() {
	rpc.Disconnect()
}

// newUpdater builds the self-updater for this binary.
func newUpdater() *updater.Updater {
	exeName := ""
	if exe, err := os.Executable(); err == nil {
		exeName = baseName(exe)
	}
	return &updater.Updater{Current: appVersion, Repo: updateRepo, ExeName: exeName}
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}

// refreshStartupItem syncs the tray checkbox with the real registry state.
func refreshStartupItem() {
	if mStartup == nil {
		return
	}
	if startup.IsEnabled() {
		mStartup.Check()
		mStartup.SetTitle("Start with Windows: On")
	} else {
		mStartup.Uncheck()
		mStartup.SetTitle("Start with Windows: Off")
	}
}

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// restartToUpdate installs the downloaded update and relaunches.
// Guarded against double-clicks launching two installers.
func restartToUpdate() {
	if installLaunched {
		return
	}
	if _, _, ok := up.Ready(); !ok {
		return
	}
	if mUpdate != nil {
		mUpdate.SetTitle("Updates: installing…")
	}
	// Tear down our subsystems so nothing races the swap script.
	rpc.Disconnect()
	if err := up.ApplyAndRestart(); err != nil {
		log.Println("Restart to update failed:", err)
		if mUpdate != nil {
			mUpdate.SetTitle("Updates: ready")
		}
		return
	}
	installLaunched = true
	// Safety net: if the helper didn't take over within 10s, offer retry.
	go func() {
		time.Sleep(10 * time.Second)
		if installLaunched {
			log.Println("Updater: install did not complete; restoring ready state.")
			installLaunched = false
			if mUpdate != nil {
				mUpdate.SetTitle("Updates: ready")
			}
		}
	}()
	systray.Quit()
}

// Detection state. Pause is a heuristic: Stremio's local API exposes no
// playhead or play/pause flag, so "no read-progress for a while" is the best
// signal available (buffering can still advance it — see README).
var (
	prevKey   string
	prevSet   bool
	lastTitle string
	pausedNow bool

	// pause inference baseline
	baseProgress float64
	baseScore    int
	hasBaseline  bool
	lastAdvance  time.Time

	// stop grace: remember the last stream briefly after it ends
	lastKnownMedia *stremio.MediaInfo
	lastSeenActive time.Time

	// poster reuse: don't re-query Cinemeta every poll for the same title
	posterFor   string
	posterValue string

	// tray elapsed clock (mirrors the Discord timer basis)
	sessionStart time.Time
	pauseBegan   time.Time
)

// shouldMarkPaused decides whether stalled progress means "Paused".
// It returns false when the heuristic is disabled (pauseAfterSec <= 0,
// the default): Stremio exposes no play/pause flag, and a fully-buffered
// stream stalls progress while still playing, so guessing Paused produces
// false "⏸ Paused" cards mid-watch. Set PAUSE_AFTER_SECONDS to opt into
// the guess (e.g. 30), knowing it can still misfire on buffered content.
func shouldMarkPaused(pauseAfterSec int, lastAdvance, now time.Time) bool {
	if pauseAfterSec <= 0 {
		return false
	}
	return now.Sub(lastAdvance) >= time.Duration(pauseAfterSec)*time.Second
}

func progressGrew(s stremio.StreamStatus) bool {
	if s.Media == nil {
		return false
	}
	if !hasBaseline {
		return true // first sighting of this title: treat as playing
	}
	if s.Progress > baseProgress {
		return true
	}
	if s.CacheScore > baseScore {
		return true
	}
	return false
}

func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	h, m, sec := s/3600, (s%3600)/60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}

func trayElapsed() string {
	if sessionStart.IsZero() {
		return ""
	}
	end := time.Now()
	if !pauseBegan.IsZero() {
		end = pauseBegan // frozen while paused/stopped
	}
	return formatElapsed(end.Sub(sessionStart))
}

func runDetection(force bool) {
	now := time.Now()
	running := stremio.IsStremioRunning()
	stremioRun = running

	var media *stremio.MediaInfo
	var status stremio.StreamStatus
	if running {
		status = stremio.FetchActiveStream()
		media = status.Media
	}
	title := ""
	if media != nil {
		title = media.Display
	}

	// ---- pause inference ----
	state := presence.StateBrowsing
	if running && media != nil {
		if title != lastTitle {
			// New title: reset baseline, assume playing.
			baseProgress, baseScore = status.Progress, status.CacheScore
			hasBaseline = true
			lastAdvance = now
			pausedNow = false
		} else if progressGrew(status) {
			baseProgress, baseScore = status.Progress, status.CacheScore
			lastAdvance = now
			pausedNow = false
		} else if !status.HasProgress && status.CacheScore == 0 {
			// No progress signals at all (e.g. direct file play with no
			// swarm data): can't infer anything, assume playing.
			pausedNow = false
		} else if shouldMarkPaused(pauseAfterSec, lastAdvance, now) {
			pausedNow = true
		}
		if pausedNow {
			state = presence.StatePaused
		} else {
			state = presence.StatePlaying
		}
		lastKnownMedia = media
		lastSeenActive = now
	} else if running && lastKnownMedia != nil && stopGraceSec > 0 &&
		now.Sub(lastSeenActive) <= time.Duration(stopGraceSec)*time.Second {
		// Stop indication: keep the last title with a Stopped card briefly
		// instead of instantly flipping to Browsing.
		media = lastKnownMedia
		title = media.Display
		state = presence.StateStopped
		pausedNow = false
		hasBaseline = false
	} else {
		if running {
			state = presence.StateBrowsing
		}
		pausedNow = false
		hasBaseline = false
	}
	if !running {
		lastKnownMedia = nil
	}
	lastTitle = title

	// ---- tray clock ----
	dispKey := string(state) + "|" + title
	if !prevSet || dispKey != trayKey {
		trayKey = dispKey
		sessionStart = now
		pauseBegan = time.Time{}
		if state == presence.StatePaused || state == presence.StateStopped {
			pauseBegan = now
		}
	} else if (state == presence.StatePaused || state == presence.StateStopped) && pauseBegan.IsZero() {
		pauseBegan = now
	} else if state == presence.StatePlaying && !pauseBegan.IsZero() {
		sessionStart = sessionStart.Add(now.Sub(pauseBegan))
		pauseBegan = time.Time{}
	}

	// ---- tray status lines (updated every poll, cheap) ----
	if mStremio != nil {
		elapsed := trayElapsed()
		suffix := ""
		if elapsed != "" && (state == presence.StatePlaying || state == presence.StatePaused) {
			suffix = " · " + elapsed
		}
		switch {
		case !running:
			mStremio.SetTitle("Stremio: not running")
		case state == presence.StatePaused:
			mStremio.SetTitle("⏸ " + truncate(title, 50) + suffix)
		case state == presence.StateStopped:
			mStremio.SetTitle("⏹ Stopped: " + truncate(title, 50))
		case title != "":
			mStremio.SetTitle("▶ " + truncate(title, 50) + suffix)
		default:
			mStremio.SetTitle("🔍 Browsing Stremio")
		}
	}
	systray.SetTooltip(fmt.Sprintf("Stremio Discord Presence\nDiscord: %s", discordStatus))

	key := fmt.Sprintf("%v|%v|%s|%s", running, privacyMode, title, state)
	changed := force || !prevSet || prevKey != key
	if !changed {
		return
	}
	prevSet, prevKey = true, key

	log.Printf("State change: running=%v privacy=%v state=%s title=%q", running, privacyMode, state, title)

	// Poster: only (re)fetch when the title changed.
	poster := ""
	if media != nil && !privacyMode && showPosters && state != presence.StateStopped {
		if title == posterFor {
			poster = posterValue
		} else {
			poster = cinemeta.GetPosterURL(media)
			posterFor, posterValue = title, poster
		}
	} else if media != nil && state == presence.StateStopped && title == posterFor {
		poster = posterValue // keep art on the stopped card
	}
	rpc.Update(running, privacyMode, media, poster, state)
}

var trayKey string

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
