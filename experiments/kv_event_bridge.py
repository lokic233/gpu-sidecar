#!/usr/bin/env python3
"""Native vLLM KV-event -> sanitized BlockEvent bridge (REFERENCE, validated on AMD MI350X).

Subscribes to a vLLM ZMQ KV-event publisher (--kv-events-config), decodes msgpack KVEventBatch
frames, SANITIZES them (SHA-256-hash the opaque block hashes, DROP raw token_ids entirely), and emits
newline-delimited JSON BlockEvents that the sidecar's cache `vllm_events` provider can ingest
(internal/cache/vllm_provider.go BlockEvent).

This is the reference consumer for the native path. The Go sidecar does NOT depend on libzmq; in a
production wiring this bridge runs as a sidecar-local helper (same host) feeding events over a local
pipe/socket into the provider's EventSource. Per-request MATCHING remains a separate, unsolved step
(documented blocker) — this only proves ingestion of real native events.

Validated 2026-06-25 against real vLLM 0.21.1 on MI350X (gfx950, ROCm 7.0.2):
  vllm serve ... --kv-events-config '{"enable_kv_cache_events":true,"publisher":"zmq",
                                      "endpoint":"tcp://*:5557","topic":"kv@mi350x"}'

Usage:
  python3 kv_event_bridge.py --endpoint tcp://127.0.0.1:5557 [--topic ""] [--out -]
"""
import argparse, hashlib, json, sys
try:
    import zmq, msgspec
except ImportError:
    sys.exit("requires pyzmq + msgspec (present in the ROCm vllm env)")


def hkey(x):
    # x is an ExternalBlockHash (int or bytes). Hash to an opaque hex key; NEVER store the raw value.
    if x is None:
        return ""
    return hashlib.sha256((x if isinstance(x, bytes) else str(x).encode())).hexdigest() \
        if isinstance(x, bytes) else hashlib.sha256(str(x).encode()).hexdigest()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", default="tcp://127.0.0.1:5557")
    ap.add_argument("--topic", default="")
    ap.add_argument("--out", default="-")
    ap.add_argument("--max", type=int, default=0, help="stop after N events (0 = forever)")
    args = ap.parse_args()

    ctx = zmq.Context.instance()
    sub = ctx.socket(zmq.SUB)
    sub.setsockopt_string(zmq.SUBSCRIBE, args.topic)
    sub.connect(args.endpoint)
    out = sys.stdout if args.out == "-" else open(args.out, "w")

    n = 0
    while True:
        parts = sub.recv_multipart()
        if len(parts) != 3:
            continue
        _topic, seq_b, payload = parts
        seq = int.from_bytes(seq_b, "big")
        batch = msgspec.msgpack.decode(payload)  # [ts, events, dp_rank]
        if not isinstance(batch, list) or len(batch) < 2:
            continue
        for e in batch[1]:
            if not isinstance(e, list) or not e:
                continue
            tag = e[0]
            if tag == "BlockStored":
                # e = [tag, block_hashes, parent_hash, token_ids(DROP), block_size, lora_id, medium,
                #      lora_name, extra_keys, group_idx, kv_cache_spec_kind, sliding_window]
                block_hashes, parent, block_size = e[1], e[2], e[4]
                for bh in block_hashes:
                    rec = {"kind": "block_stored", "seq": seq, "block_key_hash": hkey(bh),
                           "parent_key_hash": hkey(parent), "block_size": block_size}
                    out.write(json.dumps(rec) + "\n"); out.flush(); n += 1
            elif tag == "BlockRemoved":
                for bh in e[1]:
                    out.write(json.dumps({"kind": "block_removed", "seq": seq,
                                          "block_key_hash": hkey(bh)}) + "\n"); out.flush(); n += 1
            elif tag == "AllBlocksCleared":
                out.write(json.dumps({"kind": "all_blocks_cleared", "seq": seq}) + "\n"); out.flush(); n += 1
        if args.max and n >= args.max:
            break


if __name__ == "__main__":
    main()
