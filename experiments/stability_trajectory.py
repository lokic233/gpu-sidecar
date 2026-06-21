#!/usr/bin/env python3
"""Stability-score trajectory + lifecycle churn via SAFE fault injection.
Drives one device through: healthy -> injected probe failures (OFFLINE) -> cleared (RECOVERING -> READY).
Captures the stability-score time series + lifecycle transitions + recovery duration.
Requires the sidecar started with GPU_SIDECAR_FAULT_FILE=<file>.
Env: SIDECAR, DEVICE, FAULT_FILE, FAIL_SECS, RECOVER_SECS."""
import json, os, sys, time, urllib.request
SIDECAR=os.environ.get("SIDECAR","http://localhost:19095")
DEVICE=os.environ.get("DEVICE","6")
FAULT_FILE=os.environ.get("FAULT_FILE","/tmp/sidecar_fault")
FAIL_SECS=int(os.environ.get("FAIL_SECS","12"))
RECOVER_SECS=int(os.environ.get("RECOVER_SECS","40"))
POLL=0.5
def dev():
    try:
        with urllib.request.urlopen(SIDECAR+"/v1/status",timeout=4) as r:
            d=json.load(r)
            for x in d["devices"]:
                if x["identity"]["device_id"]==DEVICE: return x
    except Exception as e: return {"error":str(e)}
    return None
def main():
    t0=time.time(); samples=[]; events=[]
    open(FAULT_FILE,"w").write("clear\n")
    def rec(phase):
        x=dev()
        if x and "lifecycle_state" in x:
            samples.append({"t":round(time.time()-t0,2),"phase":phase,
                            "state":x["lifecycle_state"],"stab":round(x["stability"]["score"],4),
                            "consec_fail":x["reliability"]["consecutive_probe_failures"],
                            "avail":round(x["reliability"]["recent_availability_ratio"],3),
                            "disc":x["reliability"]["disconnect_count"],"rejoin":x["reliability"]["rejoin_count"],
                            "recovery_ms":x["reliability"]["last_recovery_duration_ms"]})
            return x["lifecycle_state"], x["stability"]["score"]
        return None,None
    # phase 1: healthy baseline
    print("# stability trajectory device",DEVICE)
    for _ in range(8): rec("healthy"); time.sleep(POLL)
    s0=samples[-1]["stab"]; print(f"baseline stab={s0:.4f} state={samples[-1]['state']}")
    # phase 2: inject probe failures
    fail_t=time.time(); open(FAULT_FILE,"w").write(f"fail {DEVICE}\n")
    print(f"INJECT fail t={fail_t-t0:.2f}")
    offline_t=None
    while time.time()-fail_t < FAIL_SECS:
        st,_=rec("failing")
        if offline_t is None and st=="OFFLINE":
            offline_t=time.time(); print(f"DETECT OFFLINE t={offline_t-t0:.2f} delay={offline_t-fail_t:.2f}")
        time.sleep(POLL)
    low=min(s["stab"] for s in samples if s["phase"]=="failing")
    print(f"min stab during failure={low:.4f}")
    # phase 3: clear fault, watch recovery
    clear_t=time.time(); open(FAULT_FILE,"w").write("clear\n")
    print(f"CLEAR fault t={clear_t-t0:.2f}")
    recovering_t=None; ready_t=None; recovered_score_t=None
    while time.time()-clear_t < RECOVER_SECS:
        st,sc=rec("recovering")
        if recovering_t is None and st=="RECOVERING":
            recovering_t=time.time(); print(f"DETECT RECOVERING t={recovering_t-t0:.2f} delay={recovering_t-clear_t:.2f}")
        if ready_t is None and st in ("READY","BUSY"):
            ready_t=time.time(); print(f"DETECT back-to-{st} t={ready_t-t0:.2f} delay={ready_t-clear_t:.2f}")
        if recovered_score_t is None and sc is not None and sc>=0.95*s0:
            recovered_score_t=time.time(); print(f"DETECT score-recovered(>=95% baseline) t={recovered_score_t-t0:.2f} delay={recovered_score_t-clear_t:.2f}")
        time.sleep(POLL)
    res={"device":DEVICE,"baseline_stab":s0,"min_stab_during_failure":low,
         "offline_detect_delay_s":round(offline_t-fail_t,2) if offline_t else None,
         "recovering_detect_delay_s":round(recovering_t-clear_t,2) if recovering_t else None,
         "back_to_ready_delay_s":round(ready_t-clear_t,2) if ready_t else None,
         "score_recovery_delay_s":round(recovered_score_t-clear_t,2) if recovered_score_t else None,
         "samples":samples}
    outdir=sys.argv[1] if len(sys.argv)>1 else "artifacts/time_series"; os.makedirs(outdir,exist_ok=True)
    fn=os.path.join(outdir,f"stability_trajectory_dev{DEVICE}_{int(t0)}.json")
    json.dump(res,open(fn,"w"),indent=2)
    print(f"SAVED {fn}")
    print(f"SUMMARY min_stab={low:.3f} offline_det={res['offline_detect_delay_s']}s recovering_det={res['recovering_detect_delay_s']}s ready={res['back_to_ready_delay_s']}s score_recovery={res['score_recovery_delay_s']}s")
if __name__=="__main__": main()
