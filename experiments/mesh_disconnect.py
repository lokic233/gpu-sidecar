#!/usr/bin/env python3
"""Mesh-level disconnect/rejoin from the COLLECTOR's perspective.
Polls a target sidecar; kills it (heartbeat loss); restarts it (detached); measures rejoin.
Captures collector-observed reachability + last_heartbeat_age over time.
Env: TARGET (sidecar url), PORT, DEVICES, BIN."""
import json, os, subprocess, sys, time, urllib.request
TARGET=os.environ.get("TARGET","http://localhost:19096")
PORT=os.environ.get("PORT","19096"); DEVICES=os.environ.get("DEVICES","7")
BIN=os.environ.get("BIN","./bin/sidecar")
def healthy():
    try:
        with urllib.request.urlopen(TARGET+"/healthz",timeout=2) as r: return r.status==200
    except Exception: return False
def main():
    t0=time.time(); samples=[]
    def rec(phase):
        h=healthy(); samples.append({"t":round(time.time()-t0,2),"phase":phase,"reachable":h}); return h
    for _ in range(6): rec("up"); time.sleep(0.5)
    print(f"initial reachable={samples[-1]['reachable']}")
    # kill it
    down=time.time(); subprocess.run(["bash","-c",f"fuser -k {PORT}/tcp"],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    print(f"GROUND_TRUTH sidecar_killed t={down-t0:.2f}")
    unreach=None
    while time.time()-down < 12:
        if not rec("down") and unreach is None:
            unreach=time.time(); print(f"DETECT unreachable t={unreach-t0:.2f} delay={unreach-down:.2f}"); break
        time.sleep(0.3)
    time.sleep(3)
    # restart fully detached (setsid + nohup) so it outlives this script
    up=time.time()
    subprocess.Popen(f"setsid nohup {BIN} -listen [::]:{PORT} -devices {DEVICES} -poll 2s >/tmp/sidecar_exp_restart.log 2>&1 < /dev/null &",
                     shell=True)
    print(f"GROUND_TRUTH sidecar_restarted t={up-t0:.2f}")
    rejoin=None
    while time.time()-up < 25:
        if rec("restart") and rejoin is None and time.time()>up+0.5:
            rejoin=time.time(); print(f"DETECT rejoin t={rejoin-t0:.2f} delay={rejoin-up:.2f}"); break
        time.sleep(0.3)
    res={"unreachable_detect_delay_s":round(unreach-down,2) if unreach else None,
         "rejoin_detect_delay_s":round(rejoin-up,2) if rejoin else None,"samples":samples}
    outdir=sys.argv[1] if len(sys.argv)>1 else "artifacts/mesh_raw"; os.makedirs(outdir,exist_ok=True)
    fn=os.path.join(outdir,f"mesh_disconnect_{int(t0)}.json"); json.dump(res,open(fn,"w"),indent=2)
    print(f"SAVED {fn}")
    print(f"SUMMARY unreachable_detect={res['unreachable_detect_delay_s']}s rejoin_detect={res['rejoin_detect_delay_s']}s")
if __name__=="__main__": main()
