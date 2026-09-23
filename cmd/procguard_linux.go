//go:build linux

package cmd

import (
	"log/slog"
	"os"
	"syscall"
)

const prSetDumpable = 4 // PR_SET_DUMPABLE

// hardenProcessForMissions stops processes running as the same (non-root)
// user, such as mission verifier commands and the agent code they execute,
// from reading this process's /proc/<pid>/environ and memory, where
// provider keys, the database DSN and the encryption key live.
func hardenProcessForMissions() {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		slog.Warn("security.missions_prctl_failed", "error", errno.Error())
	}
	if os.Geteuid() == 0 {
		slog.Warn("security.missions_running_as_root",
			"detail", "verifier commands run as root and can read gateway secrets; run the gateway as an unprivileged user until the container executor is available")
	}
}
