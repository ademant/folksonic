package bpm

import "syscall"

// lowerPriority makes the decoder yield the CPU to everything else (nice 19).
func lowerPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)
}
