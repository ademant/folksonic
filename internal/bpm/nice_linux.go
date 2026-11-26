package bpm

import "syscall"

// LowerPriority makes the decoder yield the CPU to everything else (nice 19).
// Callers pass a child they started and have not waited for yet, so pid is
// still theirs even if the child already exited (it is a zombie until Wait,
// #187); a failure, such as ESRCH, is ignored.
func LowerPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)
}
