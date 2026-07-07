//go:build linux

package supervisor

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// applyRlimits sets RLIMIT_CPU, RLIMIT_NOFILE, and RLIMIT_NPROC on the
// just-started workflow subprocess via prlimit(2). The small race window
// between cmd.Start and prlimit is acceptable for v0.1: the SDK's pipeflow
// waits for the host's Hello before user code runs, and the supervisor
// sends Hello after this call returns.
//
// Memory is deliberately NOT bounded here with RLIMIT_AS. The Go runtime
// reserves a large virtual address space at startup (the page-summary +
// heap arena, tens of GB under -race) that is mostly never resident, so a
// virtual-address-space cap crashes the child at mallocinit with "failed
// to reserve page summary memory" long before it uses any real memory. The
// memory fence is cgroup memory.max (MemoryMaxBytes, RSS-based, see
// cgroup_linux.go), with the daemon's own container/systemd unit as the
// outer fence. RLIMIT_AS here regressed all Linux workflow execution until
// it was removed; do not re-add it.
func applyRlimits(pid int, lim ResourceLimits) error {
	lim = lim.withDefaults()

	rl := &unix.Rlimit{Cur: lim.CPUSeconds, Max: lim.CPUSeconds}
	if err := unix.Prlimit(pid, unix.RLIMIT_CPU, rl, nil); err != nil {
		return fmt.Errorf("prlimit CPU: %w", err)
	}
	rl = &unix.Rlimit{Cur: lim.MaxOpenFiles, Max: lim.MaxOpenFiles}
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, rl, nil); err != nil {
		return fmt.Errorf("prlimit NOFILE: %w", err)
	}
	rl = &unix.Rlimit{Cur: lim.MaxProcesses, Max: lim.MaxProcesses}
	if err := unix.Prlimit(pid, unix.RLIMIT_NPROC, rl, nil); err != nil {
		return fmt.Errorf("prlimit NPROC: %w", err)
	}
	return nil
}
