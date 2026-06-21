#!/usr/bin/env python3
"""Crash experiment: launch a GPU worker, let it run, SIGKILL it, measure how fast the
sidecar detects the abrupt worker disappearance (worker_stop event + proc count drop)."""
import json, os, signal, subprocess, sys, time, urllib.request
SIDECAR=os.environ.get("SIDECAR","http://localhost:19095"); DEVICE=os.environ.get("DEVICE","6")
WORKLOAD=os.environ.get("WORKLOAD","./bin/workload_cuda"); VISENV=os.environ.get("VISENV","CUDA_VISIBLE_DEVICES")
MEMGB=os.environ.get("MEMGB","20"); RUN_BEFORE_KILL=int(os.environ.get("RUN_BEFORE_KILL","8"))
def st():
    try:
        with urllib.request.urlopen(SIDECAR+"/v1/status",timeout=4) as r: return json.load(r)
    except Exception as e: return {"error":str(e)}
def dv(s,d):
    for x in s.get("devices",[]):
        if x["identity"]["device_id"]==d: return x
    return None
def main():
    t0=time.time(); base=dv(st(),DEVICE); baseprocs=base["health"]["compute_proc_count"]["value"]
    env=dict(os.environ); env[VISENV]=DEVICE
    p=subprocess.Popen([WORKLOAD,"sustained","60",MEMGB],env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    print(f"GROUND_TRUTH launch pid={p.pid} t={time.time()-t0:.3f}")
    samples=[]; started=None
    # wait until detected running
    while time.time()-t0 < 25:
        d=dv(st(),DEVICE); now=time.time()
        if d:
            pr=d["health"]["compute_proc_count"]["value"] if d["health"]["compute_proc_count"]["supported"] else 0
            samples.append({"t":round(now-t0,3),"phase":"running","procs":pr,"state":d["lifecycle_state"]})
            if started is None and pr>baseprocs: started=now; print(f"DETECT start t={now-t0:.3f}")
        if started and time.time()-started>=RUN_BEFORE_KILL: break
        time.sleep(0.25)
    # SIGKILL (simulate crash)
    kill_t=time.time(); p.send_signal(signal.SIGKILL); p.wait()
    print(f"GROUND_TRUTH SIGKILL t={kill_t-t0:.3f} pid={p.pid}")
    detected=None
    while time.time()-t0 < 40:
        d=dv(st(),DEVICE); now=time.time()
        if d:
            pr=d["health"]["compute_proc_count"]["value"] if d["health"]["compute_proc_count"]["supported"] else 0
            samples.append({"t":round(now-t0,3),"phase":"killed","procs":pr,"state":d["lifecycle_state"]})
            if detected is None and pr<=baseprocs:
                detected=now; print(f"DETECT crash(proc gone) t={now-t0:.3f} delay={now-kill_t:.3f}"); break
        time.sleep(0.25)
    res={"device":DEVICE,"crash_detect_delay_s":round(detected-kill_t,3) if detected else None,"samples":samples}
    outdir=sys.argv[1] if len(sys.argv)>1 else "artifacts/time_series"; os.makedirs(outdir,exist_ok=True)
    fn=os.path.join(outdir,f"crash_dev{DEVICE}_{int(t0)}.json"); json.dump(res,open(fn,"w"),indent=2)
    print(f"SAVED {fn}\nSUMMARY crash_detect_delay={res['crash_detect_delay_s']}s")
if __name__=="__main__": main()
