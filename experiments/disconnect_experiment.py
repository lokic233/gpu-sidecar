#!/usr/bin/env python3
"""Sidecar disconnect/rejoin experiment from the COLLECTOR's perspective: poll a sidecar,
stop it (SIGTERM), observe unreachability, restart it, measure rejoin detection delay.
This validates heartbeat-loss + rejoin accounting at the mesh layer."""
import json, os, subprocess, sys, time, urllib.request
SIDECAR=os.environ.get("SIDECAR","http://localhost:19095")
SIDECAR_BIN=os.environ.get("SIDECAR_BIN","./bin/sidecar")
DEVICES=os.environ.get("SIDECAR_DEVICES","3,4,6,7"); PORT=os.environ.get("PORT","19095")
def reachable():
    try:
        with urllib.request.urlopen(SIDECAR+"/healthz",timeout=2) as r: return r.status==200
    except Exception: return False
def main():
    t0=time.time(); samples=[]
    print(f"GROUND_TRUTH initial reachable={reachable()} t={time.time()-t0:.3f}")
    # find + kill the sidecar holding PORT (do NOT broad-pkill)
    down_t=time.time()
    subprocess.run(["bash","-c",f"fuser -k {PORT}/tcp"],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    print(f"GROUND_TRUTH sidecar_stopped t={down_t-t0:.3f}")
    unreach_t=None
    while time.time()-t0<15:
        now=time.time(); r=reachable(); samples.append({"t":round(now-t0,3),"reachable":r})
        if not r and unreach_t is None: unreach_t=now; print(f"DETECT unreachable t={now-t0:.3f} delay={now-down_t:.3f}"); break
        time.sleep(0.2)
    time.sleep(2)
    # restart sidecar
    up_t=time.time()
    subprocess.Popen([SIDECAR_BIN,"-listen",f"[::]:{PORT}","-devices",DEVICES,"-poll","2s"],
                     stdout=open("/tmp/sidecar_restart.log","w"),stderr=subprocess.STDOUT)
    print(f"GROUND_TRUTH sidecar_restarted t={up_t-t0:.3f}")
    rejoin_t=None
    while time.time()-t0<30:
        now=time.time(); r=reachable(); samples.append({"t":round(now-t0,3),"reachable":r})
        if r and rejoin_t is None and now>up_t: rejoin_t=now; print(f"DETECT rejoin t={now-t0:.3f} delay={now-up_t:.3f}"); break
        time.sleep(0.2)
    res={"unreachable_detect_delay_s":round(unreach_t-down_t,3) if unreach_t else None,
         "rejoin_detect_delay_s":round(rejoin_t-up_t,3) if rejoin_t else None,"samples":samples}
    outdir=sys.argv[1] if len(sys.argv)>1 else "artifacts/mesh_raw"; os.makedirs(outdir,exist_ok=True)
    fn=os.path.join(outdir,f"disconnect_{int(t0)}.json"); json.dump(res,open(fn,"w"),indent=2)
    print(f"SAVED {fn}\nSUMMARY unreachable_detect={res['unreachable_detect_delay_s']}s rejoin_detect={res['rejoin_detect_delay_s']}s")
if __name__=="__main__": main()
