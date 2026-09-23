package sandbox

import (
	"slices"
	"testing"
)

func TestTmpfsArgs(t *testing.T) {
	got := tmpfsArgs(Config{Tmpfs: []string{"/run", "/var/tmp:size=8m"}, TmpfsExec: []string{"/tmp:size=512m,noexec", "/work"}})
	want := []string{
		"--tmpfs", "/run:noexec,nosuid,nodev",
		"--tmpfs", "/var/tmp:size=8m,noexec,nosuid,nodev",
		"--tmpfs", "/tmp:size=512m,exec,nosuid,nodev",
		"--tmpfs", "/work:exec,nosuid,nodev",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
