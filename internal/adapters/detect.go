package adapters

// Detect returns the best available adapter for this host, plus a human description.
func Detect() (Adapter, string) {
	if n := NewNVIDIA(); n.Available() {
		return n, "nvidia (nvidia-smi)"
	}
	if a := NewAMD(); a.Available() {
		return a, "amd (rocm-smi)"
	}
	return NewGeneric(), "generic (no vendor tool)"
}
