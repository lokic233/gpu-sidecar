import zmq, msgspec, threading, time, json, urllib.request, hashlib

ctx = zmq.Context.instance()
sub = ctx.socket(zmq.SUB)
sub.setsockopt(zmq.SUBSCRIBE, b"")
sub.setsockopt(zmq.RCVTIMEO, 9000)
sub.connect("tcp://127.0.0.1:5557")
time.sleep(0.5)

def h(x):  # hash a block hash (int or bytes) to opaque hex — NEVER store raw
    return hashlib.sha256(str(x).encode()).hexdigest()

def fire():
    for i in range(6):
        time.sleep(0.4)
        body = json.dumps({"model":"Qwen/Qwen2.5-0.5B-Instruct",
            "messages":[{"role":"user","content":f"sanitize capture {i} " + "token filler word " * 6 + str(time.time())}],
            "max_tokens":6}).encode()
        try: urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:8001/v1/chat/completions", data=body, headers={"Content-Type":"application/json"}), timeout=20).read()
        except Exception as e: print("fire err", e)
threading.Thread(target=fire, daemon=True).start()

out = []
try:
    while len(out) < 12:
        parts = sub.recv_multipart()
        if len(parts) != 3: continue
        topic, seq_b, payload = parts
        seq = int.from_bytes(seq_b, "big")
        raw = msgspec.msgpack.decode(payload)
        evs = raw[1] if isinstance(raw, list) and len(raw) > 1 else []
        for e in evs:
            if not isinstance(e, list) or not e: continue
            tag = e[0]
            if tag == "BlockStored":
                bhs = e[1]; parent = e[2]; block_size = e[4]
                # SANITIZE: hash block hashes, DROP token_ids (e[3]) entirely
                for bh in bhs:
                    out.append({"kind":"block_stored","seq":seq,"block_key_hash":h(bh),
                                "parent_key_hash":h(parent) if parent is not None else "","block_size":block_size})
            elif tag == "BlockRemoved":
                for bh in e[1]:
                    out.append({"kind":"block_removed","seq":seq,"block_key_hash":h(bh)})
            elif tag == "AllBlocksCleared":
                out.append({"kind":"all_blocks_cleared","seq":seq})
except zmq.error.Again:
    pass
with open("sanitized_amd_events.jsonl","w") as f:
    for o in out: f.write(json.dumps(o)+"\n")
print(f"captured {len(out)} sanitized events (token_ids DROPPED); kinds:", {k:sum(1 for o in out if o['kind']==k) for k in set(o['kind'] for o in out)})
