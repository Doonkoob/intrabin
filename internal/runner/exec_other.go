//go:build !linux

package runner

import (
	"fmt"
	"os"
)

// ExecWrapper is only supported on Linux.
func ExecWrapper(args []string) int {
	fmt.Fprintln(os.Stderr, "runner-exec is only supported on Linux")
	return 126
}
