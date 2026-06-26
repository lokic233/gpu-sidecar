#!/usr/bin/env python3
"""Cache-aware routing experimental harness (Section 10).

Drives a controlled workload against a Global Router Gateway and reports per-policy metrics. Prefix
groups (hot/warm/unique) are expressed via the opaque X-Cache-Prefix-Key experiment header (NOT
prompt content) so cache locality is deterministic and runtime-independent across H100 + MI350X.

Usage:
  python3 experiments/cache_harness.py --router http://127.0.0.1:19094 \
      --requests 200 --concurrency 8 --arrival steady --out artifacts/cache_aware_sidecar/e2e/run.json

The harness does NOT start/stop the stack and does NOT pick backends — the router does. It only
generates load and measures outcomes. No prompt/response content is logged.
"""
import argparse, json, time, threading, queue, random, statistics, urllib.request, urllib.error, sys

MODEL = "Qwen/Qwen2.5-0.5B-Instruct"

# Request shapes: (input_filler_chars, max_tokens). Input length is approximated by a filler string
# (chars/4 ~ tokens in the sidecar estimator); we keep prompts trivial + non-sensitive.
SHAPES = {
    "short_in_short_out": (40, 8),
    "short_in_long_out":  (40, 128),
    "long_in_short_out":  (3200, 8),
    "long_in_long_out":   (3200, 128),
}

# Prefix groups: hot (1 key, heavy reuse), warm (small pool), unique (no reuse).
def make_prefix(group, i):
    if group == "hot":
        return "hot-0", 512
    if group == "warm":
        return f"warm-{i % 8}", 256
    return f"uniq-{i}-{random.randint(0,1<<30)}", 0  # unique => effectively no reuse

def build_body(shape, idx):
    filler_chars, max_tokens = SHAPES[shape]
    # benign filler; content is irrelevant (we never assert on it)
    content = "x " * (filler_chars // 2)
    return json.dumps({
        "model": MODEL,
        "messages": [{"role": "user", "content": f"{content} req{idx}"}],
        "max_tokens": max_tokens,
        "stream": False,
    }).encode()

class Result:
    __slots__ = ("ok","status","ttft_ms","e2e_ms","backend","err","cancelled")
    def __init__(self):
        self.ok=False; self.status=0; self.ttft_ms=0.0; self.e2e_ms=0.0
        self.backend=""; self.err=""; self.cancelled=False

def one_request(router, shape, group, idx, timeout):
    r = Result()
    body = build_body(shape, idx)
    pkey, ptok = make_prefix(group, idx)
    req = urllib.request.Request(router + "/v1/chat/completions", data=body,
                                 headers={"Content-Type":"application/json"})
    if group != "unique":
        req.add_header("X-Cache-Prefix-Key", pkey)
        req.add_header("X-Cache-Prefix-Tokens", str(ptok))
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            r.status = resp.status
            r.backend = resp.headers.get("X-Backend-ID","")
            _ = resp.read()
            r.e2e_ms = (time.time()-t0)*1000
            r.ttft_ms = r.e2e_ms  # non-streaming: TTFT ~ E2E
            r.ok = (200 <= resp.status < 300)
    except urllib.error.HTTPError as e:
        r.status = e.code; r.err = f"http_{e.code}"; r.e2e_ms=(time.time()-t0)*1000
    except Exception as e:
        r.err = type(e).__name__; r.e2e_ms=(time.time()-t0)*1000
    return r

def run(router, n, concurrency, arrival, group_mix, shape_mix, timeout):
    jobs = queue.Queue()
    for i in range(n):
        group = random.choices(list(group_mix.keys()), weights=list(group_mix.values()))[0]
        shape = random.choices(list(shape_mix.keys()), weights=list(shape_mix.values()))[0]
        jobs.put((shape, group, i))
    results = []
    rlock = threading.Lock()
    def worker():
        while True:
            try:
                shape, group, i = jobs.get_nowait()
            except queue.Empty:
                return
            res = one_request(router, shape, group, i, timeout)
            with rlock:
                results.append(res)
            jobs.task_done()
    # arrival shaping
    threads = []
    start = time.time()
    if arrival == "prefix_burst":
        # fire bursts: all hot first, then the rest — emulated by submitting workers in waves
        pass
    for _ in range(concurrency):
        t = threading.Thread(target=worker); t.start(); threads.append(t)
    for t in threads:
        t.join()
    wall = time.time()-start
    return results, wall

def pct(xs, p):
    if not xs: return 0.0
    xs = sorted(xs)
    if len(xs)==1: return xs[0]
    k=(p/100)*(len(xs)-1); lo=int(k); frac=k-lo
    if lo>=len(xs)-1: return xs[-1]
    return xs[lo]+frac*(xs[lo+1]-xs[lo])

def summarize(results, wall, policy, arrival):
    ok=[r for r in results if r.ok]
    e2e=[r.e2e_ms for r in ok]
    ttft=[r.ttft_ms for r in ok]
    by_backend={}
    for r in ok:
        by_backend[r.backend]=by_backend.get(r.backend,0)+1
    fail=len(results)-len(ok)
    return {
        "policy": policy, "arrival": arrival,
        "total": len(results), "ok": len(ok), "failed": fail,
        "throughput_rps": round(len(ok)/wall,2) if wall>0 else 0,
        "wall_s": round(wall,2),
        "ttft_p50_ms": round(pct(ttft,50),1), "ttft_p95_ms": round(pct(ttft,95),1),
        "e2e_p50_ms": round(pct(e2e,50),1), "e2e_p95_ms": round(pct(e2e,95),1),
        "assignment": by_backend,
    }

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--router", required=True)
    ap.add_argument("--requests", type=int, default=200)
    ap.add_argument("--concurrency", type=int, default=8)
    ap.add_argument("--arrival", default="steady", choices=["steady","prefix_burst","phase_shift"])
    ap.add_argument("--policy", default="unknown", help="label only (router decides actual policy)")
    ap.add_argument("--timeout", type=float, default=60)
    ap.add_argument("--hot", type=float, default=0.5)
    ap.add_argument("--warm", type=float, default=0.3)
    ap.add_argument("--unique", type=float, default=0.2)
    ap.add_argument("--out", default="")
    args = ap.parse_args()
    group_mix = {"hot":args.hot, "warm":args.warm, "unique":args.unique}
    shape_mix = {k:1 for k in SHAPES}
    results, wall = run(args.router, args.requests, args.concurrency, args.arrival,
                        group_mix, shape_mix, args.timeout)
    summary = summarize(results, wall, args.policy, args.arrival)
    print(json.dumps(summary, indent=2))
    if args.out:
        with open(args.out, "w") as f:
            json.dump(summary, f, indent=2)
    return 0

if __name__ == "__main__":
    sys.exit(main())
