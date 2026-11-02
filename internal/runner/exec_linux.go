//go:build linux

package runner

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	// RLIMIT_NPROC counts every process of the real user ID, not only this script's. When the runner
	// shares its user with other processes (a host install, CI), a fixed limit could already be
	// exhausted and every fork would fail; so the limit allows *nproc processes beyond those that
	// already exist. In the runner container the user is dedicated and this adds almost nothing.
	limitProcs := *nproc + userProcesses(os.Getuid())
	set := func(res int, v uint64) error { return syscall.Setrlimit(res, &syscall.Rlimit{Cur: v, Max: v}) }
	const rlimitNproc = 6 // RLIMIT_NPROC on Linux
	for _, l := range []struct {
		res int
		v   uint64
	}{{syscall.RLIMIT_CPU, *cpu}, {syscall.RLIMIT_AS, *mem}, {rlimitNproc, limitProcs}, {syscall.RLIMIT_FSIZE, *fsize}, {syscall.RLIMIT_NOFILE, 256}, {syscall.RLIMIT_CORE, 0}} {
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

// userProcesses counts the tasks (threads, which is what RLIMIT_NPROC counts) of all processes
// whose real UID is uid. It returns 0 if /proc cannot be read.
func userProcesses(uid int) uint64 {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	want := strconv.Itoa(uid)
	var n uint64
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		f, err := os.Open("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		mine := false
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			switch fields[0] {
			case "Uid:":
				mine = fields[1] == want
			case "Threads:":
				if mine {
					if t, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
						n += t
					}
				}
			}
		}
		f.Close()
	}
	return n
}
