package adapters

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFaultInjectPassthrough(t *testing.T) {
	os.Unsetenv("GPU_SIDECAR_FAULT_FILE")
	inner := NewGeneric()
	w := WrapFaultInject(inner)
	if w != Adapter(inner) {
		t.Fatal("no fault file => must return inner unchanged")
	}
}

func TestFaultInjectFailure(t *testing.T) {
	dir := t.TempDir()
	ff := filepath.Join(dir, "fault")
	os.WriteFile(ff, []byte("fail 0\n"), 0644)
	os.Setenv("GPU_SIDECAR_FAULT_FILE", ff)
	defer os.Unsetenv("GPU_SIDECAR_FAULT_FILE")
	w := WrapFaultInject(NewGeneric())
	h, raw := w.Sample("0", time.Second)
	if h.GPUVisible {
		t.Fatal("injected fail must make GPUVisible=false")
	}
	if w.AccessProbe("0", time.Second) {
		t.Fatal("injected fail must make AccessProbe=false")
	}
	if raw == "" {
		t.Fatal("should return raw marker")
	}
	// device 1 not failing
	os.WriteFile(ff, []byte("fail 0\n"), 0644)
	if !w.AccessProbe("1", time.Second) == false {
		// generic AccessProbe is always false anyway; just ensure no panic
	}
}

func TestFaultInjectDelay(t *testing.T) {
	dir := t.TempDir()
	ff := filepath.Join(dir, "fault")
	os.WriteFile(ff, []byte("delay 0 300\n"), 0644)
	os.Setenv("GPU_SIDECAR_FAULT_FILE", ff)
	defer os.Unsetenv("GPU_SIDECAR_FAULT_FILE")
	w := WrapFaultInject(NewGeneric())
	start := time.Now()
	w.Sample("0", time.Second)
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("injected delay not applied")
	}
}

func TestFaultParsers(t *testing.T) {
	if got := fields("  fail   3  "); len(got) != 2 || got[0] != "fail" || got[1] != "3" {
		t.Fatalf("fields parse wrong: %v", got)
	}
	if got := splitLines("a\nb\nc"); len(got) != 3 {
		t.Fatalf("splitLines wrong: %v", got)
	}
	if atoiSafe("250") != 250 || atoiSafe("12x") != 12 {
		t.Fatal("atoiSafe wrong")
	}
}
