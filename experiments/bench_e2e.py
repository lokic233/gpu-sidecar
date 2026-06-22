#!/usr/bin/env python3
"""Benchmark the request path overhead at each hop: A=direct vLLM, B=sidecar->vLLM, C=router->sidecar->vLLM.
Measures client-observed TTFT (streaming) and end-to-end latency across concurrency levels.
Reports p50/p95/p99. Uses monotonic time for durations."""
import json, sys, time, statistics, threading, urllib.request, queue as Q

def stream_ttft_e2e(url, body):
    """Send a streaming request; return (ttft_s, e2e_s) using monotonic clock."""
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"})
    t0 = time.monotonic()
    ttft = None
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            for line in r:
                if line.startswith(b"data:") and ttft is None:
                    ttft = time.monotonic() - t0
            e2e = time.monotonic() - t0
        return ttft, e2e
    except Exception as e:
        return None, None

def run(label, url, n, concurrency, body):
    results = []
    work = Q.Queue()
    for _ in range(n): work.put(1)
    lock = threading.Lock()
    def worker():
        while True:
            try: work.get_nowait()
            except Q.Empty: return
            ttft, e2e = stream_ttft_e2e(url, body)
            if ttft is not None:
                with lock: results.append((ttft, e2e))
    threads = [threading.Thread(target=worker) for _ in range(concurrency)]
    wt0 = time.monotonic()
    for t in threads: t.start()
    for t in threads: t.join()
    wall = time.monotonic() - wt0
    if not results:
        return {"label": label, "n": 0, "error": "all failed"}
    ttfts = sorted(r[0]*1000 for r in results)  # ms
    e2es = sorted(r[1]*1000 for r in results)
    def p(a, q): 
        if not a: return 0
        k=(q/100)*(len(a)-1); lo=int(k)
        return round(a[lo] if lo>=len(a)-1 else a[lo]+(k-lo)*(a[lo+1]-a[lo]),2)
    return {"label": label, "n": len(results), "concurrency": concurrency,
            "ttft_p50_ms": p(ttfts,50), "ttft_p95_ms": p(ttfts,95), "ttft_p99_ms": p(ttfts,99),
            "e2e_p50_ms": p(e2es,50), "e2e_p95_ms": p(e2es,95),
            "throughput_rps": round(len(results)/wall,2)}

if __name__ == "__main__":
    direct = sys.argv[1] if len(sys.argv)>1 else "http://127.0.0.1:8000/v1/chat/completions"
    sidecar = sys.argv[2] if len(sys.argv)>2 else "http://127.0.0.1:19095/v1/chat/completions"
    router = sys.argv[3] if len(sys.argv)>3 else "http://127.0.0.1:19090/v1/chat/completions"
    n = int(sys.argv[4]) if len(sys.argv)>4 else 40
    body = {"model":"Qwen/Qwen2.5-0.5B-Instruct","messages":[{"role":"user","content":"Say hello briefly"}],"max_tokens":32,"stream":True}
    out = []
    for conc in [1, 8, 32]:
        for label, url in [("A_direct_vllm", direct), ("B_sidecar", sidecar), ("C_router_sidecar", router)]:
            r = run(label, url, n, conc, body)
            out.append(r)
            print(json.dumps(r))
    json.dump(out, open("/tmp/bench_results.json","w"), indent=2)
