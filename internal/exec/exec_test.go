package exec

import (
	"strings"
	"testing"
	"time"
)

func TestRunOK(t *testing.T) {
	r := Run(5*time.Second, "echo", "hello")
	if r.Err != nil || r.ExitCode != 0 { t.Fatalf("echo failed: %+v", r) }
	if !strings.Contains(string(r.Stdout), "hello") { t.Fatalf("bad stdout: %q", r.Stdout) }
}

func TestRunTimeout(t *testing.T) {
	r := Run(200*time.Millisecond, "sleep", "5")
	if !r.TimedOut { t.Fatalf("expected timeout, got %+v", r) }
	if r.ExitCode != -1 { t.Fatalf("timeout exit code should be -1, got %d", r.ExitCode) }
}

func TestRunMissingCommand(t *testing.T) {
	r := Run(2*time.Second, "this-command-does-not-exist-xyz")
	if r.Err == nil { t.Fatal("missing command should error") }
	if r.ExitCode != -1 { t.Fatalf("missing command exit -1, got %d", r.ExitCode) }
}

func TestRunNonZeroExit(t *testing.T) {
	r := Run(2*time.Second, "false")
	if r.ExitCode == 0 { t.Fatal("false should exit non-zero") }
}

func TestBoundedOutput(t *testing.T) {
	// yes floods output; ensure we bound and don't hang.
	r := Run(500*time.Millisecond, "bash", "-c", "yes AAAAAAAA | head -c 5000000")
	if len(r.Stdout) > maxOutput+4096 { t.Fatalf("output not bounded: %d", len(r.Stdout)) }
}

func TestLookPath(t *testing.T) {
	if _, ok := LookPath("echo"); !ok { t.Fatal("echo should be found") }
	if _, ok := LookPath("nope-xyz-123"); ok { t.Fatal("nonexistent should not be found") }
}
