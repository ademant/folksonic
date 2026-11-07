//go:build !linux

package bpm

// LowerPriority is a no-op outside Linux.
func LowerPriority(int) {}
