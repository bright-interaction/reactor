//go:build !linux

package supervisor

// applyRlimits is a no-op on non-Linux GOOS. macOS and FreeBSD lack the
// prlimit(2) syscall in the form Go's x/sys/unix exposes; cross-process
// rlimit enforcement only works portably on Linux. Operators on other
// platforms rely on the outer container or VM for resource bounds.
func applyRlimits(pid int, lim ResourceLimits) error {
	_ = pid
	_ = lim
	return nil
}
