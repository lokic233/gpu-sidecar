#!/usr/bin/env python3
"""Phase-shift workload (Round-5 experiment B). Runs against the cache-aware router fronting two
INDEPENDENT replicas. Phases: unique-heavy -> hot burst -> warm groups -> unique-heavy. Records, over
time, per-backend assignment, TTFT p95, and real prefix-cache hit delta per replica.
"""
import json, os, time, threading, queue, urllib.request, hashlib

URL = os.environ.get("ROUTER", "http://127.0.0.1:19094")
MODEL = "Qwen/Qwen2.5-0.5B-Instruct"
REPLICAS = {"replicaA": "http://127.0.0.1:8006", "replicaB": "http://127.0.0.1:8007"}
OUT = "artifacts/cache_aware_sidecar_hardening/results_equal/phase_shift.json"

HOT = "You are a careful assistant. " + ("Shared phase context. " * 40)
WARM = ["Warm group %d. " % i + ("warm body. " * 20) for i in range(4)]


def key(t):
    return hashlib.sha256(t.encode()).hexdigest()[:32]


def post(kind, i, timeout=60):
    if kind == "hot":
        pre, pt = HOT, len(HOT)//4
    elif kind == "warm":
        pre = WARM[i % len(WARM)]; pt = len(pre)//4
    else:
        pre, pt = "uniq-%d-%d " % (i, time.time_ns()), 0
    body = json.dumps({"model": MODEL, "messages": [{"role": "user", "content": pre + " q%d" % i}],
                       "max_tokens": 12}).encode()
    h = {"Content-Type": "application/json"}
    if kind != "unique":
        h["X-Cache-Prefix-Key"] = key(pre); h["X-Cache-Prefix-Tokens"] = str(pt)
    req = urllib.request.Request(URL + "/v1/chat/completions", data=body, headers=h)
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            r.read(); return r.headers.get("X-Backend-ID", ""), (time.time()-t0)*1000
    except Exception:
        return "", (time.time()-t0)*1000


def prefix_hits():
    out = {}
    for name, u in REPLICAS.items():
        try:
            m = urllib.request.urlopen(u + "/metrics", timeout=3).read().decode()
            for line in m.splitlines():
                if line.startswith("vllm:prefix_cache_hits_total"):
                    out[name] = float(line.split()[-1])
        except Exception:
            out[name] = -1
    return out


def run_phase(name, kinds, n, conc):
    jobs = queue.Queue()
    for i in range(n):
        jobs.put((kinds[i % len(kinds)], i))
    res = []
    lk = threading.Lock()
    def worker():
        while True:
            try:
                k, i = jobs.get_nowait()
            except queue.Empty:
                return
            bid, ms = post(k, i)
            with lk:
                res.append((bid, ms))
            jobs.task_done()
    ts = [threading.Thread(target=worker) for _ in range(conc)]
    h0 = prefix_hits()
    t0 = time.time()
    [t.start() for t in ts]; [t.join() for t in ts]
    wall = time.time()-t0
    h1 = prefix_hits()
    assign = {}
    for bid, _ in res:
        assign[bid] = assign.get(bid, 0) + 1
    e2e = sorted(m for _, m in res if m > 0)
    p95 = e2e[int(0.95*(len(e2e)-1))] if e2e else 0
    hit_delta = {k: h1.get(k, 0) - h0.get(k, 0) for k in REPLICAS}
    return {"phase": name, "n": n, "concurrency": conc, "wall_s": round(wall, 2),
            "assignment": assign, "ttft_p95_ms": round(p95, 1), "prefix_hit_delta": hit_delta}


def main():
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    conc = int(os.environ.get("CONC", "12"))
    n = int(os.environ.get("N", "120"))
    phases = [
        ("unique_heavy_1", ["unique", "unique", "unique", "warm"]),
        ("hot_burst",      ["hot", "hot", "hot", "hot"]),
        ("warm_groups",    ["warm", "warm", "warm", "unique"]),
        ("unique_heavy_2", ["unique", "unique", "unique", "warm"]),
    ]
    out = []
    for name, kinds in phases:
        row = run_phase(name, kinds, n, conc)
        out.append(row)
        print(f"  {name:<16} assign={row['assignment']} ttft_p95={row['ttft_p95_ms']}ms hit_delta={row['prefix_hit_delta']}")
    json.dump(out, open(OUT, "w"), indent=2)
    print("wrote", OUT)


if __name__ == "__main__":
    main()
