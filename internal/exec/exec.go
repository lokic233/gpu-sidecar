// Package exec runs vendor CLI tools defensively: hard timeout, captured exit code,
// bounded output. A hung vendor command must never hang the sidecar.
package exec

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// Result is the bounded outcome of a command run.
type Result struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	Err       error // non-nil on timeout, spawn failure, or non-zero exit
	Duration  time.Duration
	TimedOut  bool
	Truncated bool
}

// maxOutput caps captured stdout/stderr to keep memory bounded.
const maxOutput = 1 << 20 // 1 MiB

// boundedBuffer caps how much we keep.
type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len() >= b.limit {
		b.truncated = true
		return len(p), nil // discard but report success so the child keeps draining
	}
	remaining := b.limit - b.buf.Len()
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

// Run executes name+args with a hard timeout. It always returns a Result (never panics).
func Run(timeout time.Duration, name string, args ...string) Result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb boundedBuffer
	out.limit, errb.limit = maxOutput, maxOutput
	cmd.Stdout = &out
	cmd.Stderr = &errb

	runErr := cmd.Run()
	dur := time.Since(start)

	r := Result{
		Stdout:    out.buf.Bytes(),
		Stderr:    errb.buf.Bytes(),
		Duration:  dur,
		Truncated: out.truncated || errb.truncated,
	}
	if ctx.Err() == context.DeadlineExceeded {
		r.TimedOut = true
		r.Err = context.DeadlineExceeded
		r.ExitCode = -1
		return r
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			r.ExitCode = ee.ExitCode()
		} else {
			r.ExitCode = -1 // spawn failure (command not found, etc.)
		}
		r.Err = runErr
		return r
	}
	r.ExitCode = 0
	return r
}

// RunCtx is like Run but with a caller-supplied context for cancellation.
func RunCtx(ctx context.Context, timeout time.Duration, name string, args ...string) Result {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(cctx, name, args...)
	var out, errb boundedBuffer
	out.limit, errb.limit = maxOutput, maxOutput
	cmd.Stdout = &out
	cmd.Stderr = &errb
	runErr := cmd.Run()
	r := Result{Stdout: out.buf.Bytes(), Stderr: errb.buf.Bytes(), Duration: time.Since(start), Truncated: out.truncated || errb.truncated}
	if cctx.Err() == context.DeadlineExceeded {
		r.TimedOut, r.Err, r.ExitCode = true, context.DeadlineExceeded, -1
		return r
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			r.ExitCode = ee.ExitCode()
		} else {
			r.ExitCode = -1
		}
		r.Err = runErr
		return r
	}
	return r
}

// LookPath reports whether a tool is on PATH (for graceful degradation).
func LookPath(name string) (string, bool) {
	p, err := exec.LookPath(name)
	return p, err == nil
}
