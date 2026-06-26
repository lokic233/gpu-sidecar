#!/usr/bin/env python3
"""Policy comparison driver (Section 10). Restarts the router under each policy and runs the harness,
collecting a comparison table. Run from the repo root on the H100 node where the router lives.

It compares: round_robin, least_queued, health_gated_least_pressure, cache_affinity_only,
cache_aware_estimated_finish. The sidecars + vLLM stay up across runs; only the router changes.
"""
import json, os, subprocess, sys, time, signal, urllib.request

ROUTER_BIN = "./bin/router"
ROUTER_ADDR = "127.0.0.1:19094"
ROUTER_URL = "http://" + ROUTER_ADDR
COLLECTOR = "http://127.0.0.1:29110/v1/events"
BACKENDS = json.dumps([
    {"id":"h100-gpu3","vendor":"nvidia","sidecar_url":"http://127.0.0.1:19097","snapshot_url":"http://127.0.0.1:19097"},
    {"id":"mi350x-gpu2","vendor":"amd","sidecar_url":"http://[2401:db00:272c:590b:face:0:133:0]:19097","snapshot_url":"http://[2401:db00:272c:590b:face:0:133:0]:19097"},
])
POLICIES = ["round_robin","least_queued","health_gated_least_pressure","cache_affinity_only","cache_aware_estimated_finish"]
OUTDIR = "artifacts/cache_aware_sidecar/e2e/comparison"

def kill_router():
    subprocess.run(["pkill","-f", f"{ROUTER_BIN} -listen {ROUTER_ADDR}"], capture_output=True)
    time.sleep(1.5)

def start_router(policy):
    log = open(f"{OUTDIR}/router_{policy}.log","w")
    p = subprocess.Popen([ROUTER_BIN,"-listen",ROUTER_ADDR,"-backends",BACKENDS,
                          "-policy",policy,"-snapshot-interval","300ms",
                          "-collector-url",COLLECTOR,"-max-retries","1"],
                         stdout=log, stderr=subprocess.STDOUT)
    # wait until it serves
    for _ in range(40):
        try:
            urllib.request.urlopen(ROUTER_URL+"/healthz", timeout=1).read()
            time.sleep(1.0)  # let one snapshot materialize
            return p
        except Exception:
            time.sleep(0.25)
    return p

def run_harness(policy, requests, concurrency, arrival):
    out = f"{OUTDIR}/{policy}_{arrival}.json"
    r = subprocess.run([sys.executable,"experiments/cache_harness.py","--router",ROUTER_URL,
                        "--requests",str(requests),"--concurrency",str(concurrency),
                        "--policy",policy,"--arrival",arrival,"--out",out,
                        "--hot","0.5","--warm","0.3","--unique","0.2"],
                       capture_output=True, text=True, timeout=600)
    if r.returncode != 0:
        print(f"  harness failed: {r.stderr[-300:]}")
        return None
    return json.load(open(out))

def main():
    os.makedirs(OUTDIR, exist_ok=True)
    requests = int(os.environ.get("REQS","160"))
    concurrency = int(os.environ.get("CONC","12"))
    arrival = os.environ.get("ARRIVAL","steady")
    table = []
    for pol in POLICIES:
        print(f"=== {pol} ===")
        kill_router()
        p = start_router(pol)
        time.sleep(1.0)
        summary = run_harness(pol, requests, concurrency, arrival)
        if summary:
            table.append(summary)
            print(f"  rps={summary['throughput_rps']} e2e_p50={summary['e2e_p50_ms']} "
                  f"e2e_p95={summary['e2e_p95_ms']} assign={summary['assignment']}")
        try:
            p.terminate()
        except Exception:
            pass
        time.sleep(1.0)
    with open(f"{OUTDIR}/comparison.json","w") as f:
        json.dump(table, f, indent=2)
    print("\n=== COMPARISON TABLE ===")
    print(f"{'policy':<32}{'rps':>8}{'e2e_p50':>10}{'e2e_p95':>10}  assignment")
    for row in table:
        print(f"{row['policy']:<32}{row['throughput_rps']:>8}{row['e2e_p50_ms']:>10}{row['e2e_p95_ms']:>10}  {row['assignment']}")
    return 0

if __name__ == "__main__":
    sys.exit(main())
