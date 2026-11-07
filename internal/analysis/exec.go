package analysis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tsirysndr/tinysonic/internal/bpm"
)

// maxToolOutput bounds what is kept of a tool's stdout and stderr.
const maxToolOutput = 16 << 20

// capped is a writer that keeps at most max bytes and drops the rest.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// runTool runs command with args at the lowest CPU priority and returns its
// stdout and stderr. A non-zero exit is an error unless okExit accepts it.
func runTool(ctx context.Context, command string, args []string, okExit func(int) bool) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, command, args...)
	out, errOut := &capped{max: maxToolOutput}, &capped{max: maxToolOutput}
	cmd.Stdout, cmd.Stderr = out, errOut
	// A child the tool started may keep the pipes open after the tool is
	// killed on timeout; do not wait for it longer than this.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	bpm.LowerPriority(cmd.Process.Pid)
	err = cmd.Wait()
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && okExit != nil && okExit(exit.ExitCode())) {
		return nil, nil, fmt.Errorf("%v: %s", err, shortMessage(errOut.buf.String()))
	}
	return out.buf.Bytes(), errOut.buf.Bytes(), nil
}

// shortMessage is the last line of a tool's error output, at most 200 bytes.
func shortMessage(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
