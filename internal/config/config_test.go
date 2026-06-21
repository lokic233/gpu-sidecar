package config

import "testing"

func TestDefaultIsLoopbackOnly(t *testing.T) {
	c := Default()
	if !IsLoopback(c.ListenAddr) {
		t.Fatalf("default listen address must be loopback-only, got %q", c.ListenAddr)
	}
	if c.ListenAddr != "127.0.0.1:9095" {
		t.Fatalf("default should be 127.0.0.1:9095, got %q", c.ListenAddr)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:9095":  true,
		"127.0.0.1:19095": true,
		"[::1]:9095":       true,
		"localhost:9095":   true,
		"127.5.5.5:80":     true,
		"[::]:9095":        false, // all interfaces
		"0.0.0.0:9095":     false,
		"192.168.1.10:9095": false,
		"[2401:db00::1]:9095": false,
	}
	for addr, want := range cases {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q)=%v want %v", addr, got, want)
		}
	}
}
