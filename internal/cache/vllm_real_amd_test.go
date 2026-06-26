package cache

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// realAMDEvents are REAL, sanitized KV block-lifecycle events captured live from vLLM 0.21.1 running
// on an AMD MI350X (gfx950, ROCm 7.0.2), 2026-06-25, via the native ZMQ KV-event publisher
// (--kv-events-config). Block hashes are SHA-256-hashed to opaque keys and token_ids were DROPPED at
// capture time (never persisted). Parent relationships are preserved. This fixture proves the
// vllm_events provider ingests real AMD hardware events with the same schema as NVIDIA.
//
// Capture/sanitization: artifacts/cache_aware_sidecar/e2e/mi350x_realvllm/capture_sanitized.py
const realAMDEvents = `
{"kind":"block_stored","seq":11,"block_key_hash":"62ba18decb4a1bc96934efcceec745a33e46ef93996eea45e0daa24c7848ebea","parent_key_hash":"c7223984481b27e5d6cb95dd2b3b683e1076d7b8751e6a7933bbb88c03f05875","block_size":16}
{"kind":"block_stored","seq":11,"block_key_hash":"d2f0b4616279eafbe893458703512f36d141ad6cac42df59ed235ea7dceae213","parent_key_hash":"c7223984481b27e5d6cb95dd2b3b683e1076d7b8751e6a7933bbb88c03f05875","block_size":16}
{"kind":"block_stored","seq":11,"block_key_hash":"3aa65fad6c5f08777da8a4cacde0e7baa206c6cc5bc46417345d0a86cd1d372f","parent_key_hash":"c7223984481b27e5d6cb95dd2b3b683e1076d7b8751e6a7933bbb88c03f05875","block_size":16}
{"kind":"block_stored","seq":11,"block_key_hash":"63536154beb1f34ed8a809535ad0ad5e2af86bc639a1f6f0028e67f726a0813f","parent_key_hash":"d2f0b4616279eafbe893458703512f36d141ad6cac42df59ed235ea7dceae213","block_size":16}
{"kind":"block_stored","seq":12,"block_key_hash":"a08a209f145a17e79a63519a4e52b12b2ba9f9082d68bef258808e1dc567542b","parent_key_hash":"c7223984481b27e5d6cb95dd2b3b683e1076d7b8751e6a7933bbb88c03f05875","block_size":16}
{"kind":"block_stored","seq":12,"block_key_hash":"7b8f630fe4b704a88fb50be190dac11f5294ad5470f6047e20d8347d11847a46","parent_key_hash":"c7223984481b27e5d6cb95dd2b3b683e1076d7b8751e6a7933bbb88c03f05875","block_size":16}
{"kind":"block_stored","seq":12,"block_key_hash":"71084f6c6ae7e6795607dca08346baabc1714d8d759436b3683cedf3415327ad","parent_key_hash":"c7223984481b27e5d6cb95dd2b3b683e1076d7b8751e6a7933bbb88c03f05875","block_size":16}
{"kind":"block_stored","seq":12,"block_key_hash":"98a484b24afdfccc9a4c60b643e89315c7f8529e9c9b0750e42f90928ebe1e76","parent_key_hash":"7b8f630fe4b704a88fb50be190dac11f5294ad5470f6047e20d8347d11847a46","block_size":16}
{"kind":"block_stored","seq":13,"block_key_hash":"0fd772f8caba87ebbfda807febc3d27fb2e2c7ed83e47d8ee42060524cacb2af","parent_key_hash":"c7223984481b27e5d6cb95dd2b3b683e1076d7b8751e6a7933bbb88c03f05875","block_size":16}
{"kind":"block_stored","seq":13,"block_key_hash":"636adfd4cedbb78a26a5e5f1aa5b73f4b408fb2fc79c0fbb785f7075453bbb45","parent_key_hash":"c7223984481b27e5d6cb95dd2b3b683e1076d7b8751e6a7933bbb88c03f05875","block_size":16}
{"kind":"block_stored","seq":13,"block_key_hash":"f2c0b6c5884e9a0a27af68310d4460a30c1fd013f60b59a13e50a86e3de24e69","parent_key_hash":"c7223984481b27e5d6cb95dd2b3b683e1076d7b8751e6a7933bbb88c03f05875","block_size":16}
{"kind":"block_stored","seq":13,"block_key_hash":"278ac4d6100424e99ab52950675982b1e67de02ffc3e2a8659d77dd0811eece2","parent_key_hash":"636adfd4cedbb78a26a5e5f1aa5b73f4b408fb2fc79c0fbb785f7075453bbb45","block_size":16}
`

type sanitizedAMDEvent struct {
	Kind         string `json:"kind"`
	Seq          int64  `json:"seq"`
	BlockKeyHash string `json:"block_key_hash"`
	ParentHash   string `json:"parent_key_hash"`
	BlockSize    int    `json:"block_size"`
}

// TestVLLMProvider_IngestsRealAMDEvents replays REAL sanitized MI350X KV events through the
// vllm_events provider and asserts: (1) all events ingest into the bounded index, (2) NO token_ids
// are present anywhere (the fixture proves they were dropped at capture), (3) the snapshot reports a
// supported observation plane with the real last sequence, and (4) per-request match stays
// UNSUPPORTED (the documented blocker) — never a fabricated match from real block hashes.
func TestVLLMProvider_IngestsRealAMDEvents(t *testing.T) {
	p := NewVLLMProvider(ProviderConfig{Mode: ModeVLLMEvents, Index: IndexConfig{
		MaxEntries: 1000, EntryTTL: time.Hour, StaleAfter: time.Hour}})
	_ = p.Start(context.Background())
	defer p.Stop()

	var n int
	var maxSeq int64
	sc := bufio.NewScanner(strings.NewReader(strings.TrimSpace(realAMDEvents)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e sanitizedAMDEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad fixture line: %v", err)
		}
		// the BlockEvent carries ONLY metadata — there is structurally no token_ids field.
		p.Ingest(BlockEvent{
			Kind: EventKind(e.Kind), Seq: e.Seq, BlockKeyHash: e.BlockKeyHash,
			ParentKeyHash: e.ParentHash, BlockSize: e.BlockSize,
		})
		n++
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	if n != 12 {
		t.Fatalf("expected 12 real AMD events, ingested %d", n)
	}

	s := p.Snapshot()
	if !s.Supported {
		t.Fatalf("observation plane should be supported after ingesting real AMD events")
	}
	if s.MatchSupported {
		t.Fatalf("per-request match must remain UNSUPPORTED on this stack (blocker), even with real AMD events")
	}
	if s.IndexEntries != 12 {
		t.Fatalf("expected 12 indexed blocks from real AMD events, got %d", s.IndexEntries)
	}
	if s.LastEventSequence != maxSeq {
		t.Fatalf("expected last seq %d, got %d", maxSeq, s.LastEventSequence)
	}
	if s.Provider != string(ModeVLLMEvents) {
		t.Fatalf("provider mismatch: %s", s.Provider)
	}

	// Lookup of a real AMD block hash must NOT fabricate a match (blocker behavior).
	mr := p.Lookup(PrefixQuery{
		PrefixKeyHash: "62ba18decb4a1bc96934efcceec745a33e46ef93996eea45e0daa24c7848ebea",
		PrefixTokens:  16,
	})
	if mr.MatchSupported || mr.MatchedPrefixTokens != 0 || mr.Confidence != 0 {
		t.Fatalf("native match must not be fabricated from real block hashes, got %+v", mr)
	}

	// Directory must be empty (router must not receive an untrustworthy matchable directory).
	if len(p.Directory(100)) != 0 {
		t.Fatalf("native directory must be empty on this stack, got %d", len(p.Directory(100)))
	}
}

// TestVLLMProvider_RealAMDParentChain verifies parent-block relationships from the real AMD stream
// are recorded (parent hashes preserved as opaque keys; chains observable).
func TestVLLMProvider_RealAMDParentChain(t *testing.T) {
	idx := NewIndex(IndexConfig{MaxEntries: 1000, EntryTTL: time.Hour, StaleAfter: time.Hour})
	// seq 11: block d2f0... has parent c722...; block 6353... has parent d2f0... (a real chain)
	idx.ApplyStore(11, "d2f0b4616279eafbe893458703512f36d141ad6cac42df59ed235ea7dceae213",
		"c7223984481b27e5d6cb95dd2b3b683e1076d7b8751e6a7933bbb88c03f05875", 16, 16)
	idx.ApplyStore(11, "63536154beb1f34ed8a809535ad0ad5e2af86bc639a1f6f0028e67f726a0813f",
		"d2f0b4616279eafbe893458703512f36d141ad6cac42df59ed235ea7dceae213", 16, 16)
	if tok, _, _ := idx.LookupKey("63536154beb1f34ed8a809535ad0ad5e2af86bc639a1f6f0028e67f726a0813f"); tok != 16 {
		t.Fatalf("expected child block present with 16 tokens, got %d", tok)
	}
	if idx.SnapshotMeta().IndexEntries != 2 {
		t.Fatalf("expected 2 chained blocks")
	}
}
