package bpm

import "syscall"

// LowerPriority makes the decoder yield the CPU to everything else (nice 19).
func LowerPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)
}
