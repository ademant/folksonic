//go:build linux

package analysis

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// applyToolLimits runs on a tool's child process right after it starts.
//
// The child is made the first candidate for the kernel's OOM killer
// (oom_score_adj 1000), so when memory runs out the tool is killed before
// folksonic or another service on the host. The write is best effort: the
// run goes on without it. A positive memLimit caps the child's data segment
// (RLIMIT_DATA), so an allocation past it fails inside the tool.
func applyToolLimits(pid int, memLimit int64) error {
	_ = os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte("1000"), 0)
	if memLimit <= 0 {
		return nil
	}
	rl := syscall.Rlimit{Cur: uint64(memLimit), Max: uint64(memLimit)}
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(pid),
		uintptr(syscall.RLIMIT_DATA), uintptr(unsafe.Pointer(&rl)), 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("memory_limit: setting RLIMIT_DATA: %v", errno)
	}
	return nil
}
