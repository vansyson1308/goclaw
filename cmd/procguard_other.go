//go:build !linux

package cmd

import "log/slog"

func hardenProcessForMissions() {
	slog.Warn("security.missions_process_guard_unavailable",
		"detail", "verifier commands run as the gateway user and may read its environment on this platform")
}
