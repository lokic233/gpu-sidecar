#!/usr/bin/env python3
"""Equal-capability policy comparison — Round-5 HARDENED (P0 #2).

REQUIRES two GENUINELY INDEPENDENT vLLM replicas (independent process / GPU / port / KV cache /
scheduler). It REFUSES to run when both backend definitions resolve to the same vLLM runtime identity
(process_start_time_seconds, surfaced as runtime_instance_id via the router /v1/backends). Two sidecars
over one runtime is NOT a two-replica cache-routing experiment.

The two backends must be supplied as independent sidecars (each fronting its own vLLM). See
launch_two_replicas.sh (two H100 GPUs) + independent_replica_proof.md.
"""
import json, os, subprocess, sys, time, urllib.request

ROUTER_BIN = "./bin/router"
ADDR = "127.0.0.1:19094"
URL = "http://" + ADDR
COLLECTOR = os.environ.get("COLLECTOR", "http://127.0.0.1:29110/v1/events")
# Two INDEPENDENT sidecars (default ports; override via env). Each fronts its own vLLM replica.
BACKENDS = os.environ.get("BACKENDS", json.dumps([
    {"id": "replicaA", "vendor": "nvidia", "sidecar_url": "http://127.0.0.1:19101", "snapshot_url": "http://127.0.0.1:19101"},
    {"id": "replicaB", "vendor": "nvidia", "sidecar_url": "http://127.0.0.1:19102", "snapshot_url": "http://127.0.0.1:19102"},
]))
POLICIES = ["round_robin", "least_queued", "health_gated_least_pressure", "cache_affinity_only", "cache_aware_estimated_finish"]
OUTDIR = "artifacts/cache_aware_sidecar_hardening/results_equal"
MODEL = os.environ.get("MODEL", "Qwen/Qwen2.5-0.5B-Instruct")


def kill_router():
    subprocess.run(["pkill", "-f", f"{ROUTER_BIN} -listen {ADDR}"], capture_output=True)
    time.sleep(1.5)


def start_router(policy):
    os.makedirs(OUTDIR, exist_ok=True)
    log = open(f"{OUTDIR}/router_{policy}.log", "w")
    p = subprocess.Popen([ROUTER_BIN, "-listen", ADDR, "-backends", BACKENDS, "-policy", policy,
                          "-snapshot-interval", "300ms", "-collector-url", COLLECTOR, "-max-retries", "1"],
                         stdout=log, stderr=subprocess.STDOUT)
    for _ in range(40):
        try:
            urllib.request.urlopen(URL + "/healthz", timeout=1).read()
            time.sleep(1.2)
            return p
        except Exception:
            time.sleep(0.25)
    return p


def assert_independent_replicas():
    """HARD STOP: refuse if the two backends resolve to the same vLLM runtime identity."""
    backends = json.load(urllib.request.urlopen(URL + "/v1/backends", timeout=5))["backends"]
    if len(backends) < 2:
        sys.exit("HARD STOP: need >=2 backends, got %d" % len(backends))
    ids = {}
    for b in backends:
        bid = b["backend"]["id"]
        # robust runtime identity: endpoint id (host+vllm-url+boot). Fall back to instance id.
        rid = b.get("runtime_endpoint_id") or ("inst:%s" % b.get("runtime_instance_id", 0))
        reachable = b.get("reachable") and b.get("runtime_healthy")
        if not reachable:
            sys.exit(f"HARD STOP: backend {bid} not reachable/healthy — cannot prove independence")
        if not b.get("runtime_endpoint_id") and not b.get("runtime_instance_id"):
            sys.exit(f"HARD STOP: backend {bid} exposes no runtime identity — cannot prove independent runtime")
        if rid in ids:
            sys.exit(f"HARD STOP: backends {ids[rid]} and {bid} share runtime identity {rid} "
                     f"(SAME vLLM process/KV cache). This is NOT a two-replica experiment. Refusing to run.")
        ids[rid] = bid
    print(f"independent-replica check PASSED: distinct runtime identities {list(ids.keys())}")
    return ids


def post(body, headers, timeout=60):
    req = urllib.request.Request(URL + "/v1/chat/completions", data=body,
                                 headers={"Content-Type": "application/json", **headers})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            r.read()
            return r.status, r.headers.get("X-Backend-ID", ""), (time.time() - t0) * 1000
    except Exception:
        return 0, "", (time.time() - t0) * 1000


# Hot/warm prefixes are REAL identical prompt prefixes (see experiment_protocol.md). The explicit key
# is derived from the same shared prefix text, so synthetic key == real shared content.
HOT_PREFIX = "You are a meticulous assistant. " + ("Shared context block. " * 40)
WARM_PREFIXES = ["Warm pool entry %d. " % i + ("warm filler. " * 20) for i in range(8)]


def shared_key(text):
    import hashlib
    return hashlib.sha256(text.encode()).hexdigest()[:32]


def make_request(kind, i):
    if kind == "hot":
        prefix, ptoks = HOT_PREFIX, len(HOT_PREFIX) // 4
    elif kind == "warm":
        prefix = WARM_PREFIXES[i % len(WARM_PREFIXES)]
        ptoks = len(prefix) // 4
    else:
        prefix, ptoks = "unique-%d-%d " % (i, time.time_ns()), 0
    body = json.dumps({"model": MODEL,
                       "messages": [{"role": "user", "content": prefix + " Q%d: reply briefly." % i}],
                       "max_tokens": 16}).encode()
    headers = {}
    if kind != "unique":
        headers = {"X-Cache-Prefix-Key": shared_key(prefix), "X-Cache-Prefix-Tokens": str(ptoks)}
    return body, headers


import threading, queue, random
def run_load(n, conc, timeout=60):
    jobs = queue.Queue()
    for i in range(n):
        r = random.random()
        kind = "hot" if r < 0.6 else ("warm" if r < 0.8 else "unique")
        jobs.put((kind, i))
    res = []
    lk = threading.Lock()
    def worker():
        while True:
            try:
                kind, i = jobs.get_nowait()
            except queue.Empty:
                return
            body, headers = make_request(kind, i)
            st, bid, ms = post(body, headers, timeout)
            with lk:
                res.append((kind, st, bid, ms))
            jobs.task_done()
    ts = [threading.Thread(target=worker) for _ in range(conc)]
    t0 = time.time(); [t.start() for t in ts]; [t.join() for t in ts]
    return res, time.time() - t0


def pct(xs, p):
    if not xs:
        return 0
    xs = sorted(xs); k = (p/100)*(len(xs)-1); lo = int(k); f = k-lo
    return xs[-1] if lo >= len(xs)-1 else xs[lo]+f*(xs[lo+1]-xs[lo])


def main():
    os.makedirs(OUTDIR, exist_ok=True)
    n = int(os.environ.get("REQS", "160"))
    concs = [int(x) for x in os.environ.get("CONCS", "1,4,8,16,32").split(",")]
    table = []
    for pol in POLICIES:
        kill_router()
        p = start_router(pol)
        time.sleep(0.8)
        assert_independent_replicas()  # HARD STOP if shared runtime
        for conc in concs:
            res, wall = run_load(n, conc)
            ok = [r for r in res if 200 <= r[1] < 300]
            e2e = [r[3] for r in ok]
            assign = {}
            for r in ok:
                assign[r[2]] = assign.get(r[2], 0) + 1
            hot = [r for r in ok if r[0] == "hot"]
            hot_assign = {}
            for r in hot:
                hot_assign[r[2]] = hot_assign.get(r[2], 0) + 1
            row = {"policy": pol, "concurrency": conc, "ok": len(ok), "failed": len(res)-len(ok),
                   "throughput_rps": round(len(ok)/wall, 2) if wall > 0 else 0,
                   "e2e_p50_ms": round(pct(e2e, 50), 1), "e2e_p95_ms": round(pct(e2e, 95), 1),
                   "e2e_p99_ms": round(pct(e2e, 99), 1), "assignment": assign, "hot_assignment": hot_assign}
            table.append(row)
            print(f"  {pol:32s} c={conc:<3d} rps={row['throughput_rps']:<7} "
                  f"e2e_p50={row['e2e_p50_ms']:<7} p95={row['e2e_p95_ms']:<8} assign={assign}")
        try:
            p.terminate()
        except Exception:
            pass
        time.sleep(0.8)
    json.dump(table, open(f"{OUTDIR}/comparison.json", "w"), indent=2)
    print("\nwrote", f"{OUTDIR}/comparison.json")
    return 0


if __name__ == "__main__":
    sys.exit(main())
