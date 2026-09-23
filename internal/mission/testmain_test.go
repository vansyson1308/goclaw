package mission

import (
	"os"
	"testing"
)

// sharedGoCache is one Go build cache for every check in this package's
// tests; production checks each get a fresh cache (see HostExecutor.GoCache).
var sharedGoCache string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "goclaw-mission-gocache-")
	if err != nil {
		panic(err)
	}
	sharedGoCache = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func testExecutor() HostExecutor { return HostExecutor{GoCache: sharedGoCache} }
