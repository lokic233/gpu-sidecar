#!/usr/bin/env python3
"""Vendor-agnostic detection experiment. Launches a controlled workload and AUTO-DETECTS which
device the sidecar observes changing (handles AMD's HIP-ordinal != rocm-smi-card mapping).
Measures detection delay for: worker start (proc/mem), BUSY transition, worker stop.

Env: SIDECAR, WORKLOAD, VISENV, VISDEV (device to pin the workload to via VISENV),
     SECS, MEMGB, EXCLUDE (comma device_ids to ignore e.g. busy GPUs)."""
import json, os, subprocess, sys, time, urllib.request
SIDECAR=os.environ.get("SIDECAR","http://localhost:19095")
WORKLOAD=os.environ.get("WORKLOAD","./bin/workload_cuda")
VISENV=os.environ.get("VISENV","CUDA_VISIBLE_DEVICES")
VISDEV=os.environ.get("VISDEV","4")
SECS=int(os.environ.get("SECS","14")); MEMGB=os.environ.get("MEMGB","20")
EXCLUDE=set(os.environ.get("EXCLUDE","").split(",")) - {""}
POLL=0.25; MEM_THRESH=int(float(MEMGB)*0.4*1e9)  # 40% of requested as detection threshold

def status():
    try:
        with urllib.request.urlopen(SIDECAR+"/v1/status",timeout=4) as r: return json.load(r)
    except Exception as e: return {"error":str(e),"devices":[]}

def snap(st):
    out={}
    for d in st.get("devices",[]):
        i=d["identity"]["device_id"]
        if i in EXCLUDE: continue
        h=d["health"]
        out[i]={"mem":h["mem_used_bytes"]["value"] if h["mem_used_bytes"]["supported"] else 0,
                "procs":h["compute_proc_count"]["value"] if h["compute_proc_count"]["supported"] else 0,
                "state":d["lifecycle_state"],
                "util":h["utilization_gpu_pct"]["value"] if h["utilization_gpu_pct"]["supported"] else 0,
                "stab":d["stability"]["score"],"effcap":d["effective_capacity"]}
    return out

def main():
    t0=time.time()
    base=snap(status())
    print(f"# auto-detect workload={WORKLOAD} pin={VISENV}={VISDEV} excl={sorted(EXCLUDE)}")
    env=dict(os.environ); env[VISENV]=VISDEV
    launch=time.time()
    proc=subprocess.Popen([WORKLOAD,"sustained",str(SECS),str(MEMGB)],env=env,
                          stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    print(f"GROUND_TRUTH launch t={launch-t0:.3f} pid={proc.pid}")
    affected=None; start_det=None; busy_det=None; stop_t=None; stop_det=None
    samples=[]
    while time.time()-t0 < SECS+25:
        now=time.time(); cur=snap(status()); samples.append({"t":round(now-t0,3),"dev":cur})
        # find affected device: mem grew significantly vs baseline
        if affected is None:
            for i,v in cur.items():
                b=base.get(i,{"mem":0,"procs":0})
                if v["mem"]-b["mem"]>MEM_THRESH or v["procs"]>b["procs"]:
                    affected=i; start_det=now
                    print(f"DETECT worker_start dev={i} t={now-t0:.3f} delay={now-launch:.3f} (mem+{(v['mem']-b['mem'])/1e9:.1f}GB procs={v['procs']})")
                    break
        elif busy_det is None and cur.get(affected,{}).get("state") in ("BUSY","DEGRADED"):
            busy_det=now
            print(f"DETECT state->{cur[affected]['state']} dev={affected} t={now-t0:.3f} delay={now-launch:.3f}")
        if proc.poll() is not None and stop_t is None:
            stop_t=time.time(); print(f"GROUND_TRUTH exit t={stop_t-t0:.3f} code={proc.returncode}")
        if stop_t is not None and affected is not None and stop_det is None:
            v=cur.get(affected,{"procs":0,"mem":0}); b=base.get(affected,{"procs":0,"mem":0})
            if v["procs"]<=b["procs"] and v["mem"]-b["mem"]<MEM_THRESH:
                stop_det=now; print(f"DETECT worker_stop dev={affected} t={now-t0:.3f} delay={now-stop_t:.3f}"); break
        time.sleep(POLL)
    res={"workload":WORKLOAD,"affected_device":affected,
         "start_delay_s":round(start_det-launch,3) if start_det else None,
         "busy_delay_s":round(busy_det-launch,3) if busy_det else None,
         "stop_delay_s":round(stop_det-stop_t,3) if (stop_det and stop_t) else None,
         "poll_s":POLL,"samples":samples}
    outdir=sys.argv[1] if len(sys.argv)>1 else "artifacts/time_series"; os.makedirs(outdir,exist_ok=True)
    fn=os.path.join(outdir,f"autodetect_{os.path.basename(WORKLOAD)}_{int(t0)}.json")
    json.dump(res,open(fn,"w"),indent=2)
    print(f"SAVED {fn}")
    print(f"SUMMARY affected_dev={affected} start={res['start_delay_s']}s busy={res['busy_delay_s']}s stop={res['stop_delay_s']}s")
if __name__=="__main__": main()
