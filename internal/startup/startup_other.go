//go:build !windows

package startup

// Stubs for non-Windows builds (the app is Windows-first; the detector has a
// pgrep fallback, but autostart registration is Windows-only for now).

func IsEnabled() bool       { return false }
func SetEnabled(bool) bool  { return false }
func MigrateLegacyEntries() {}
