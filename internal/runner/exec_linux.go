//go:build linux

package runner

import (
	"flag"
	"fmt"
	"os"
	"syscall"
)

// ExecWrapper implements `intrabin runner-exec`: it applies resource limits to itself and then
// replaces itself with the interpreter, so the limits apply to the script and its children.
func ExecWrapper(args []string) int {
	fs := flag.NewFlagSet("runner-exec", flag.ContinueOnError)
	cpu := fs.Uint64("cpu", 60, "CPU seconds")
	mem := fs.Uint64("mem", 512<<20, "address space bytes")
	nproc := fs.Uint64("nproc", 64, "max processes")
	fsize := fs.Uint64("fsize", 64<<20, "max file size bytes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "runner-exec: missing command")
		return 2
	}
	set := func(res int, v uint64) error { return syscall.Setrlimit(res, &syscall.Rlimit{Cur: v, Max: v}) }
	const rlimitNproc = 6 // RLIMIT_NPROC on Linux
	for _, l := range []struct {
		res int
		v   uint64
	}{{syscall.RLIMIT_CPU, *cpu}, {syscall.RLIMIT_AS, *mem}, {rlimitNproc, *nproc}, {syscall.RLIMIT_FSIZE, *fsize}, {syscall.RLIMIT_NOFILE, 256}, {syscall.RLIMIT_CORE, 0}} {
		if err := set(l.res, l.v); err != nil {
			fmt.Fprintf(os.Stderr, "runner-exec: setrlimit: %v\n", err)
			return 126
		}
	}
	if err := syscall.Exec(rest[0], rest, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "runner-exec: exec %s: %v\n", rest[0], err)
		return 127
	}
	return 0
}
