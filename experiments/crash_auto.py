#!/usr/bin/env python3
"""Vendor-agnostic SIGKILL crash detection using MEMORY-delta (robust on AMD where per-card
proc-count is unreliable). Launches a worker, lets it allocate, SIGKILLs it, measures how fast
the sidecar sees the memory drop on the affected device. Env: SIDECAR,WORKLOAD,VISENV,VISDEV,MEMGB,EXCLUDE,RUN_BEFORE_KILL"""
import json, os, signal, subprocess, sys, time, urllib.request
SIDECAR=os.environ.get("SIDECAR","http://localhost:19095"); WORKLOAD=os.environ.get("WORKLOAD","./bin/workload_hip")
VISENV=os.environ.get("VISENV","HIP_VISIBLE_DEVICES"); VISDEV=os.environ.get("VISDEV","4")
MEMGB=os.environ.get("MEMGB","25"); EXCLUDE=set(os.environ.get("EXCLUDE","").split(","))-{""}
RUN=int(os.environ.get("RUN_BEFORE_KILL","6")); MEMT=int(float(MEMGB)*0.4*1e9)
def snap():
    try:
        with urllib.request.urlopen(SIDECAR+"/v1/status",timeout=4) as r:
            d=json.load(r)
            return {x["identity"]["device_id"]:(x["health"]["mem_used_bytes"]["value"] if x["health"]["mem_used_bytes"]["supported"] else 0)
                    for x in d["devices"] if x["identity"]["device_id"] not in EXCLUDE}
    except Exception: return {}
def main():
    t0=time.time(); base=snap(); env=dict(os.environ); env[VISENV]=VISDEV
    p=subprocess.Popen([WORKLOAD,"sustained","40",MEMGB],env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    print(f"GROUND_TRUTH launch pid={p.pid} t={time.time()-t0:.2f}")
    aff=None; started=None; samples=[]
    while time.time()-t0<25:
        cur=snap(); now=time.time(); samples.append({"t":round(now-t0,2),"phase":"run","mem":cur})
        if aff is None:
            for i,m in cur.items():
                if m-base.get(i,0)>MEMT: aff=i; started=now; print(f"DETECT start dev={i} t={now-t0:.2f} (+{(m-base.get(i,0))/1e9:.1f}GB)"); break
        if started and now-started>=RUN: break
        time.sleep(0.25)
    kt=time.time(); p.send_signal(signal.SIGKILL); p.wait(); print(f"GROUND_TRUTH SIGKILL t={kt-t0:.2f}")
    det=None
    while time.time()-t0<45:
        cur=snap(); now=time.time(); samples.append({"t":round(now-t0,2),"phase":"killed","mem":cur})
        if aff and det is None and cur.get(aff,0)-base.get(aff,0)<MEMT:
            det=now; print(f"DETECT crash(mem freed) dev={aff} t={now-t0:.2f} delay={now-kt:.2f}"); break
        time.sleep(0.25)
    res={"device":aff,"crash_detect_delay_s":round(det-kt,2) if det else None,"method":"memory-delta","samples":samples}
    outdir=sys.argv[1] if len(sys.argv)>1 else "artifacts/mi350x_raw"; os.makedirs(outdir,exist_ok=True)
    fn=os.path.join(outdir,f"crash_auto_{int(t0)}.json"); json.dump(res,open(fn,"w"),indent=2)
    print(f"SAVED {fn}\nSUMMARY crash_detect={res['crash_detect_delay_s']}s (memory-based) dev={aff}")
if __name__=="__main__": main()
