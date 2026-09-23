//go:build linux

package mission

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// killProcessesUnder SIGKILLs every process whose working directory is
// inside dir: work an attempt started (including detached `nohup`/`setsid`
// children of exec) must not keep running after the attempt ends. A process
// that changed its directory elsewhere is not found; the container executor
// is the complete answer to that.
func killProcessesUnder(dir string) []int {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var killed []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil {
			continue
		}
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		if cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator)) {
			if syscall.Kill(pid, syscall.SIGKILL) == nil {
				killed = append(killed, pid)
			}
		}
	}
	return killed
}
