// Package startup registers the app to launch at Windows login.
//
// It mirrors the Electron app's startupManager.js: the exe path is stored in
// HKCU\Software\Microsoft\Windows\CurrentVersion\Run, and the
// StartupApproved sibling value is kept in sync so Task Manager's Startup tab
// shows the real state instead of a stale entry. Entries left by older
// (Electron) builds are removed on startup.
package startup

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	// Name is the Run value name. Kept identical to the Electron app so the
	// toggle and Task Manager state carry over across the rewrite.
	Name = "Stremio Discord Presence"

	runKey           = `Software\Microsoft\Windows\CurrentVersion\Run`
	approvedKey      = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
	approvedEnabled  = "020000000000000000000000"
	approvedDisabled = "030000000000000000000000"
)

// legacyNames are Run values written by older builds; removed on startup.
var legacyNames = []string{
	"electron.app.Stremio Discord Presence",
	"StremioDiscordPresence",
	"com.kavishalakshan.stremiodiscordpresence",
}

// exeCommand is the command stored in the Run value: "C:\path\app.exe" --hidden.
// --hidden is accepted (and ignored) by the app; it keeps the entry
// compatible with the Electron version's login items.
func exeCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// os.Executable may return a resolved path; keep it absolute and quoted.
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	return `"` + exe + `" --hidden`, nil
}

// IsEnabled reports whether launch-at-login is currently registered AND
// pointing at this exe. A stale entry pointing elsewhere counts as disabled
// (SetEnabled(true) will overwrite it).
func IsEnabled() bool {
	want, err := exeCommand()
	if err != nil {
		return false
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	got, _, err := k.GetStringValue(Name)
	if err != nil {
		return false
	}
	return sameCommand(got, want)
}

func sameCommand(a, b string) bool {
	na := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(a), "--hidden")))
	nb := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(b), "--hidden")))
	na = strings.Trim(na, `"`)
	nb = strings.Trim(nb, `"`)
	return na != "" && na == nb
}

// SetEnabled turns launch-at-login on or off and returns the resulting state
// (read back, so callers show the truth, not the request).
func SetEnabled(enabled bool) bool {
	if enabled {
		cmd, err := exeCommand()
		if err != nil {
			return false
		}
		k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
		if err != nil {
			return false
		}
		if err := k.SetStringValue(Name, cmd); err != nil {
			k.Close()
			return false
		}
		k.Close()
		setApproved(true)
	} else {
		k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
		if err != nil {
			return IsEnabled()
		}
		_ = k.DeleteValue(Name) // absent already -> already disabled
		k.Close()
		setApproved(false)
	}
	return IsEnabled()
}

// setApproved mirrors Task Manager's enable/disable flag. Deleting the value
// also reads as "enabled" to Task Manager, but writing it explicitly keeps
// the tab from showing a stale "Disabled" row after re-enabling.
func setApproved(enabled bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, approvedKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	raw := approvedDisabled
	if enabled {
		raw = approvedEnabled
	}
	_ = k.SetBinaryValue(Name, hexToBytes(raw))
}

func hexToBytes(s string) []byte {
	b := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		var v byte
		for _, c := range s[i : i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				v |= byte(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v |= byte(c-'A') + 10
			}
		}
		b = append(b, v)
	}
	return b
}

// MigrateLegacyEntries removes Run/Approved values from older builds.
// If a legacy entry was enabled, the current exe takes over (same behavior
// as the Electron app's migrateLegacyEntries).
func MigrateLegacyEntries() {
	legacyWasEnabled := false
	for _, name := range legacyNames {
		if deleteValue(runKey, name) {
			legacyWasEnabled = true
		}
		deleteValue(approvedKey, name)
	}
	if legacyWasEnabled && !IsEnabled() {
		SetEnabled(true)
	}
}

func deleteValue(keyPath, name string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	if _, _, err := k.GetStringValue(name); err != nil {
		if _, _, err := k.GetBinaryValue(name); err != nil {
			return false
		}
	}
	return k.DeleteValue(name) == nil
}
