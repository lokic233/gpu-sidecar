package cache

// PublicSnapshot is the JSON body served at GET /v1/cache. It embeds the bounded metadata Snapshot
// plus an optional opaque directory (hashed prefix-key -> matched tokens) the router materializes
// off the hot path. NO raw content, NO unhashed keys, NO token ids.
type PublicSnapshot struct {
	Snapshot
	// Directory maps opaque (hashed) prefix keys to matched token counts. Present only for
	// match-capable providers (explicit mode); empty for native-events-on-this-stack and disabled.
	// Bounded by the provider's directory cap.
	Directory map[string]int `json:"directory,omitempty"`
}

// BuildPublicSnapshot assembles the /v1/cache body from a provider. dirMax bounds the directory size.
func BuildPublicSnapshot(p Provider, dirMax int) PublicSnapshot {
	return PublicSnapshot{
		Snapshot:  p.Snapshot(),
		Directory: p.Directory(dirMax),
	}
}
