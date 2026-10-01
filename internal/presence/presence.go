// Package presence manages the Discord Rich Presence connection.
// Ported from src/discordRpc.js, using hugolgst/rich-go over local IPC.
package presence

import (
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/hugolgst/rich-go/client"
	"github.com/macromaster101/stremio-discord-presence-go/internal/stremio"
)

// PlayState is the inferred player state. Stremio exposes no play/pause API,
// so Paused is a heuristic (no read-progress for a while); the rest is exact.
type PlayState string

const (
	StatePlaying  PlayState = "playing"
	StatePaused   PlayState = "paused"
	StateStopped  PlayState = "stopped"
	StateBrowsing PlayState = "browsing"
)

// Manager holds RPC state. Use New then Connect / Update / Disconnect.
type Manager struct {
	mu             sync.Mutex
	clientID       string
	connected      bool
	reconnectTimer *time.Timer
	startTime      time.Time
	pausedSince    time.Time
	lastKey        string
	onStatus       func(string)
}

// New creates a Manager for the given Discord application (client) ID.
func New(clientID string) *Manager {
	return &Manager{clientID: clientID}
}

// OnStatus registers a status listener (Connected / Disconnected / ...).
func (m *Manager) OnStatus(fn func(string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onStatus = fn
}

func (m *Manager) emit(status string) {
	fn := m.onStatus
	if fn != nil {
		// call without holding the lock to avoid deadlocks
		go fn(status)
	}
}

// Connected reports the current connection state.
func (m *Manager) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// Connect dials the local Discord client. Safe to call repeatedly; it
// schedules a 15s retry on failure like the original.
func (m *Manager) Connect() {
	m.mu.Lock()
	if m.connected {
		m.mu.Unlock()
		return
	}
	if m.clientID == "" {
		m.mu.Unlock()
		m.emit("Missing Client ID")
		return
	}
	if m.reconnectTimer != nil {
		m.reconnectTimer.Stop()
		m.reconnectTimer = nil
	}
	m.mu.Unlock()

	m.emit("Connecting...")
	if err := client.Login(m.clientID); err != nil {
		m.scheduleReconnect()
		return
	}
	m.mu.Lock()
	m.connected = true
	m.mu.Unlock()
	m.emit("Connected")
}

func (m *Manager) scheduleReconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = false
	m.emit("Disconnected")
	if m.reconnectTimer != nil {
		return
	}
	m.reconnectTimer = time.AfterFunc(15*time.Second, func() {
		m.mu.Lock()
		m.reconnectTimer = nil
		m.mu.Unlock()
		m.Connect()
	})
}

// Disconnect closes the connection and cancels pending reconnects.
func (m *Manager) Disconnect() {
	m.mu.Lock()
	if m.reconnectTimer != nil {
		m.reconnectTimer.Stop()
		m.reconnectTimer = nil
	}
	was := m.connected
	m.connected = false
	m.mu.Unlock()
	if was {
		client.Logout()
	}
	m.emit("Disconnected")
}

// Clear removes the current activity (Stremio closed).
func (m *Manager) Clear() {
	m.mu.Lock()
	m.startTime = time.Time{}
	m.pausedSince = time.Time{}
	m.lastKey = ""
	connected := m.connected
	m.mu.Unlock()
	if !connected {
		return
	}
	// An empty activity clears the display; a send error means Discord went
	// away, so fall back to reconnect logic.
	if err := client.SetActivity(client.Activity{}); err != nil {
		m.handleSendError()
	}
}

func (m *Manager) handleSendError() {
	// Best effort: drop the broken socket so the next Connect redials.
	client.Logout()
	m.scheduleReconnect()
}

// Update pushes presence for the current Stremio state.
//
// Timer policy (the "timer" improvement):
//   - Playing / Browsing / Privacy: Discord counts up from startTime.
//   - Paused / Stopped: no timestamp is sent, so Discord shows a frozen card
//     instead of a running clock. Resume shifts startTime forward so paused
//     time is excluded from the elapsed total.
func (m *Manager) Update(isRunning, privacyMode bool, media *stremio.MediaInfo, posterURL string, state PlayState) {
	m.mu.Lock()
	connected := m.connected
	m.mu.Unlock()
	if !connected {
		return
	}
	if !isRunning {
		m.Clear()
		return
	}

	act := client.Activity{}
	buttons := []*client.Button{
		{Label: "Get Stremio", Url: "https://www.stremio.com/downloads"},
	}

	title := ""
	if media != nil {
		title = media.Display
	}
	key := string(state) + "|" + title
	if privacyMode {
		key = "privacy"
	} else if media == nil && state == StateBrowsing {
		key = "browsing"
	}

	if privacyMode {
		act.Details = "▶ Watching Stremio"
		act.State = "Privacy mode on"
		act.LargeImage = "stremio"
		act.LargeText = "Stremio"
		act.Buttons = buttons[:1]
		m.tickKey(key, state, true)
		if st := m.stamp(); !st.IsZero() {
			act.Timestamps = &client.Timestamps{Start: &st}
		}
	} else if media == nil {
		if state == StateStopped {
			// Should not happen (caller passes last media for stopped),
			// but degrade gracefully instead of showing an empty card.
			act.Details = "Browsing Stremio"
			act.State = "Looking for something to watch"
		} else {
			act.Details = "Browsing Stremio"
			act.State = "🔍 Looking for something to watch"
		}
		act.LargeImage = "stremio"
		act.LargeText = "Stremio"
		act.Buttons = buttons[:1]
		m.tickKey(key, StateBrowsing, true)
		if st := m.stamp(); !st.IsZero() {
			act.Timestamps = &client.Timestamps{Start: &st}
		}
	} else {
		paused := state == StatePaused
		stopped := state == StateStopped
		if media.Type == "series" {
			ep := fmt.Sprintf("S%02dE%02d", media.Season, media.Episode)
			if media.AbsoluteEpisode {
				ep = fmt.Sprintf("E%02d", media.Episode)
			}
			epFull := ep
			if media.EpisodeTitle != "" {
				epFull = ep + " · " + media.EpisodeTitle
			}
			act.Details = media.Name
			switch {
			case stopped:
				act.State = "⏹ Stopped · " + ep
			case paused:
				act.State = "⏸ Paused · " + epFull
			default:
				act.State = "▶ " + epFull
			}
		} else {
			if media.Year != "" {
				act.Details = media.Name + " (" + media.Year + ")"
			} else {
				act.Details = media.Name
			}
			switch {
			case stopped:
				act.State = "⏹ Stopped watching"
			case paused:
				act.State = "⏸ Paused"
			default:
				act.State = "▶ Watching a movie"
			}
		}

		if media.SearchName != "" {
			buttons = append(buttons, &client.Button{
				Label: "Search Title",
				Url:   "https://www.google.com/search?q=" + url.QueryEscape(media.SearchName),
			})
		}
		if len(buttons) > 2 {
			buttons = buttons[:2] // Discord allows max 2
		}
		act.Buttons = buttons

		if posterURL != "" {
			act.LargeImage = posterURL
			if title != "" {
				act.LargeText = title
			} else {
				act.LargeText = "Stremio"
			}
			act.SmallImage = "stremio"
			switch {
			case stopped:
				act.SmallText = "Stopped"
			case paused:
				act.SmallText = "Paused"
			default:
				act.SmallText = "Playing"
			}
		} else {
			act.LargeImage = "stremio"
			act.LargeText = "Stremio"
		}

		// Timer: frozen for paused/stopped, running otherwise.
		m.tickKey(key, state, !paused && !stopped)
		if !paused && !stopped {
			if st := m.stamp(); !st.IsZero() {
				act.Timestamps = &client.Timestamps{Start: &st}
			}
		}
	}

	if err := client.SetActivity(act); err != nil {
		m.handleSendError()
	}
}

// tickKey maintains the elapsed-timer basis across state changes.
func (m *Manager) tickKey(key string, state PlayState, running bool) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if key != m.lastKey {
		m.startTime = now
		m.pausedSince = time.Time{}
		m.lastKey = key
		return
	}
	if running {
		if !m.pausedSince.IsZero() {
			// Resumed: exclude the paused interval from elapsed.
			m.startTime = m.startTime.Add(now.Sub(m.pausedSince))
			m.pausedSince = time.Time{}
		}
		if m.startTime.IsZero() {
			m.startTime = now
		}
	} else if m.pausedSince.IsZero() {
		m.pausedSince = now
	}
}

func (m *Manager) stamp() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startTime
}
