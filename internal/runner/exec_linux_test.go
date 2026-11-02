//go:build linux

package runner

import (
	"os"
	"testing"
)

func TestUserProcessesCountsThisProcess(t *testing.T) {
	if n := userProcesses(os.Getuid()); n < 1 {
		t.Fatalf("userProcesses = %d, want at least this test's own threads", n)
	}
	if n := userProcesses(1 << 30); n != 0 {
		t.Fatalf("unused UID has %d tasks", n)
	}
}
