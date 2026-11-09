//go:build !linux

package analysis

import "errors"

// applyToolLimits has no effect off Linux: the OOM score and the memory
// limit are Linux interfaces. A configured memory_limit fails the run, so the
// limit is never silently ignored.
func applyToolLimits(_ int, memLimit int64) error {
	if memLimit > 0 {
		return errors.New("memory_limit: supported on Linux only")
	}
	return nil
}
