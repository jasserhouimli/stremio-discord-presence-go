# Stremio Discord Presence (Go)

Lightweight Windows tray companion that shows your Stremio status on Discord
Rich Presence. A minimal Go rewrite of the
[Electron original](https://github.com/MacroMaster101/Stremio_Discord_Rich_Presence)
— same core behavior, ~10 MB single `.exe` instead of ~200 MB.

## What it does (core only)

- Detects Stremio Desktop (`stremio-shell-ng.exe` / `stremio.exe` via `tasklist`)
- Reads the active stream from Stremio's local server (`127.0.0.1:11470/stats.json`),
  picking the actually-playing torrent/file (not a stale cached one)
- Parses torrent/file names: `Series · SxxExx – Episode`, anime `E60`, `Movie (Year)`,
  with release-group/codec/quality tags stripped
- Discord Rich Presence over local IPC: title, poster art, "Get Stremio" +
  "Search Title" buttons, 15 s auto-reconnect
- Player states with timer policy: ▶ Playing (running timer), ⏸ Paused (frozen
  card, pause time excluded on resume), ⏹ Stopped (keeps the last title briefly
  after a stream ends), 🔍 Browsing (own timer). Privacy Mode now also carries
  a timer (previously missing).
- Better detection: stale torrents lingering in stats after the player closes
  no longer show a stuck title; adaptive polling (fast while playing, slow
  while idle); posters fetched once per title, not every poll.
- Optional poster art from Stremio's public Cinemeta addon (no API key, cached)
- Tray menu: Discord/Stremio status with state icons + elapsed time, Privacy
  Mode toggle, poster toggle, **Start with Windows** toggle (registry Run key
  kept in sync with Task Manager, legacy Electron entries cleaned up),
  Reconnect Discord, **Check for Updates** with tray progress + Restart to
  install, Quit. Single instance.

Omitted on purpose: auto-updater, start-with-Windows, About window,
theme-aware icons.

## Run

Requirements: Windows, Discord desktop client, Stremio Desktop, Go 1.21+ (to build).

```powershell
cd stremio-discord-presence-go
go build -o stremio-discord-presence.exe .
.\stremio-discord-presence.exe
```

Optional `.env` next to the exe (see `.env.example`):

| Variable                  | Default | Description                                   |
| ------------------------- | ------- | --------------------------------------------- |
| `POLL_INTERVAL_MS`        | `5000`  | Idle check interval (min 1000)                  |
| `ACTIVE_POLL_INTERVAL_MS` | `2500`  | Check interval while playing (min 1000)         |
| `PAUSE_AFTER_SECONDS`     | `0`     | Stalled seconds before guessing ⏸ (`0` = off) |
| `STOP_GRACE_SECONDS`      | `120`   | How long the ⏹ card stays (`0` disables)       |
| `PRIVACY_MODE`            | `false` | Start with generic "Watching Stremio"           |
| `UPDATE_REPO`             | original repo | `owner/name` to check for updates         |
| `UPDATE_CHECK`            | `true`  | Silent update check after launch                |

> Pause honesty: Stremio's local API exposes no playhead or play/pause flag,
> so ⏸ can only ever be a guess — and a fully-buffered stream looks identical
> to a paused one (no progress movement while you keep watching). Guessing
> wrong shows a false "⏸ Paused" card mid-watch, so auto-pause is **off by
> default** and the card shows ▶ Playing for any active stream. Opt in with
> `PAUSE_AFTER_SECONDS` (e.g. `30`) only if you prefer a sometimes-wrong
> pause indicator over always-Playing.

Privacy Mode broadcasts only "Watching Stremio" and skips poster lookups.
Poster art sends only the title to `v3-cinemeta.strem.io`; turn it off in the
tray for zero external calls.

## Layout

```text
main.go                  # tray + poll loop + .env + single instance
icon.ico                 # tray icon (embedded)
internal/stremio/        # process check, stats.json, title parsing
internal/cinemeta/       # poster lookup + cache
internal/presence/       # Discord RPC manager (rich-go)
internal/startup/        # launch at login (Windows Run key)
internal/updater/        # self-update from GitHub Releases
```

## Updates & startup

- **Start with Windows** writes `"exe" --hidden` to `HKCU\...\Run` and syncs
  the `StartupApproved` flag so Task Manager's Startup tab shows the true
  state. First launch removes leftover entries from the Electron builds.
- **Check for Updates** compares `appVersion` against the latest GitHub tag
  (sans `v`), downloads the exe asset with tray progress, verifies it looks
  like a real executable, then swaps it over the running binary via a helper
  script and relaunches. A silent check runs ~8s after launch; only
  available/ready states surface tray changes.
- Releasing: bump `appVersion` in `main.go`, attach the built exe to the
  GitHub release (the updater prefers an asset matching the exe name, then a
  `*go*.exe`, then any Stremio exe). Pushing to `main` with a new version
  auto-tags `v<version>` and publishes the release via
  `.github/workflows/release.yml` — no manual steps.

## Verify

```powershell
go vet ./...
go test ./...
```
