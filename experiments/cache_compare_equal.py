#!/usr/bin/env python3
"""Equal-capability policy comparison (Section 10, clean isolation).

Two backends of IDENTICAL speed (both H100 sidecars -> same fast vLLM). The ONLY asymmetry is cache
locality, induced via the explicit-prefix experiment header. This isolates the routing-policy effect:
  - round_robin / least_queued: ignore locality -> split hot-prefix traffic across both (miss reuse)
  - cache_affinity_only: herd ALL hot traffic onto whichever backend warmed first (ignores load)
  - cache_aware_estimated_finish: send hot traffic to the warm backend UNTIL it gets congested, then
    balance -> best of both.

We measure per-backend assignment for the HOT prefix group specifically, plus aggregate latency.
"""
import json, os, subprocess, sys, time, urllib.request

ROUTER_BIN="./bin/router"; ADDR="127.0.0.1:19094"; URL="http://"+ADDR
COLLECTOR="http://127.0.0.1:29110/v1/events"
BACKENDS=json.dumps([
  {"id":"h100a","vendor":"nvidia","sidecar_url":"http://127.0.0.1:19097","snapshot_url":"http://127.0.0.1:19097"},
  {"id":"h100b","vendor":"nvidia","sidecar_url":"http://127.0.0.1:19098","snapshot_url":"http://127.0.0.1:19098"},
])
POLICIES=["round_robin","least_queued","health_gated_least_pressure","cache_affinity_only","cache_aware_estimated_finish"]
OUTDIR="artifacts/cache_aware_sidecar/e2e/comparison_equal"
MODEL="Qwen/Qwen2.5-0.5B-Instruct"

def kill_router():
    subprocess.run(["pkill","-f",f"{ROUTER_BIN} -listen {ADDR}"],capture_output=True); time.sleep(1.5)

def start_router(policy):
    log=open(f"{OUTDIR}/router_{policy}.log","w")
    p=subprocess.Popen([ROUTER_BIN,"-listen",ADDR,"-backends",BACKENDS,"-policy",policy,
                        "-snapshot-interval","300ms","-collector-url",COLLECTOR,"-max-retries","1"],
                       stdout=log,stderr=subprocess.STDOUT)
    for _ in range(40):
        try:
            urllib.request.urlopen(URL+"/healthz",timeout=1).read(); time.sleep(1.2); return p
        except Exception: time.sleep(0.25)
    return p

def post(body, headers, timeout=60):
    req=urllib.request.Request(URL+"/v1/chat/completions",data=body,
        headers={"Content-Type":"application/json",**headers})
    t0=time.time()
    try:
        with urllib.request.urlopen(req,timeout=timeout) as r:
            r.read(); return r.status, r.headers.get("X-Backend-ID",""), (time.time()-t0)*1000
    except Exception as e:
        return 0, "", (time.time()-t0)*1000

def warm(prefix, tokens):
    # one request to establish locality somewhere (router picks; we then read directories)
    body=json.dumps({"model":MODEL,"messages":[{"role":"user","content":"warm "+("x "*20)}],"max_tokens":4}).encode()
    post(body,{"X-Cache-Prefix-Key":prefix,"X-Cache-Prefix-Tokens":str(tokens)})

def which_backend_has(prefix):
    import hashlib
    h=hashlib.sha256(prefix.encode()).hexdigest()
    out=[]
    for bid,url in [("h100a","http://127.0.0.1:19097"),("h100b","http://127.0.0.1:19098")]:
        try:
            d=json.load(urllib.request.urlopen(url+"/v1/cache",timeout=2))
            if h in d.get("directory",{}): out.append(bid)
        except Exception: pass
    return out

import threading, queue, random
def run_load(n, conc, hot_prefix, hot_tokens, timeout=60):
    jobs=queue.Queue()
    for i in range(n):
        # 60% hot prefix, 40% unique
        if random.random()<0.6: jobs.put(("hot",i))
        else: jobs.put(("uniq",i))
    res=[]; lk=threading.Lock()
    def worker():
        while True:
            try: kind,i=jobs.get_nowait()
            except queue.Empty: return
            body=json.dumps({"model":MODEL,"messages":[{"role":"user","content":("x "*20)+f" {i}"}],"max_tokens":16}).encode()
            if kind=="hot":
                st,bid,ms=post(body,{"X-Cache-Prefix-Key":hot_prefix,"X-Cache-Prefix-Tokens":str(hot_tokens)},timeout)
            else:
                st,bid,ms=post(body,{},timeout)
            with lk: res.append((kind,st,bid,ms))
            jobs.task_done()
    ts=[threading.Thread(target=worker) for _ in range(conc)]
    t0=time.time();[t.start() for t in ts];[t.join() for t in ts];wall=time.time()-t0
    return res,wall

def pct(xs,p):
    if not xs:return 0
    xs=sorted(xs);k=(p/100)*(len(xs)-1);lo=int(k);f=k-lo
    return xs[-1] if lo>=len(xs)-1 else xs[lo]+f*(xs[lo+1]-xs[lo])

def main():
    os.makedirs(OUTDIR,exist_ok=True)
    n=int(os.environ.get("REQS","160")); conc=int(os.environ.get("CONC","12"))
    hot_prefix="ISOHOT"; hot_tokens=1024
    table=[]
    for pol in POLICIES:
        print(f"=== {pol} ===")
        kill_router(); p=start_router(pol); time.sleep(1.0)
        # warm the hot prefix on whichever backend the router picks first (establishes locality)
        warm(hot_prefix,hot_tokens); time.sleep(0.8)
        warmed=which_backend_has(hot_prefix)
        res,wall=run_load(n,conc,hot_prefix,hot_tokens)
        ok=[r for r in res if 200<=r[1]<300]
        hot=[r for r in ok if r[0]=="hot"]
        hot_assign={}
        for r in hot: hot_assign[r[2]]=hot_assign.get(r[2],0)+1
        all_assign={}
        for r in ok: all_assign[r[2]]=all_assign.get(r[2],0)+1
        e2e=[r[3] for r in ok]
        # cache-hot concentration = fraction of HOT-prefix requests that hit the warmed backend
        conc_frac=0.0
        if hot and warmed:
            hits=sum(v for b,v in hot_assign.items() if b in warmed)
            conc_frac=hits/len(hot)
        row={"policy":pol,"ok":len(ok),"failed":len(res)-len(ok),
             "throughput_rps":round(len(ok)/wall,2) if wall>0 else 0,
             "e2e_p50_ms":round(pct(e2e,50),1),"e2e_p95_ms":round(pct(e2e,95),1),
             "warmed_backend":warmed,"hot_assignment":hot_assign,"all_assignment":all_assign,
             "hot_prefix_concentration":round(conc_frac,3)}
        table.append(row)
        print(f"  rps={row['throughput_rps']} e2e_p50={row['e2e_p50_ms']} e2e_p95={row['e2e_p95_ms']} "
              f"warmed={warmed} hot_assign={hot_assign} conc={row['hot_prefix_concentration']}")
        try: p.terminate()
        except Exception: pass
        time.sleep(1.0)
    json.dump(table,open(f"{OUTDIR}/comparison.json","w"),indent=2)
    print("\n=== EQUAL-CAPABILITY COMPARISON (cache locality = only asymmetry) ===")
    print(f"{'policy':<32}{'rps':>7}{'e2e_p50':>9}{'e2e_p95':>9}{'hot_conc':>9}  hot_assign")
    for r in table:
        print(f"{r['policy']:<32}{r['throughput_rps']:>7}{r['e2e_p50_ms']:>9}{r['e2e_p95_ms']:>9}{r['hot_prefix_concentration']:>9}  {r['hot_assignment']}")
    return 0

if __name__=="__main__": sys.exit(main())
