#!/usr/bin/env python3
"""Round-2 correctness-hardening validation. Exercises the NEW semantics and records, for each
phase: lifecycle state, reason codes, soft-failure counter, /readyz verdict, stability score.
Distinguishes GROUND TRUTH (harness actions) from SIDECAR-OBSERVED signals.

Env: SIDECAR, FAULT_FILE, WORKLOAD, VISENV, VISDEV, EXCLUDE, NODE_LABEL, OUTDIR."""
import json, os, signal, subprocess, sys, time, urllib.request
SIDECAR=os.environ.get("SIDECAR","http://localhost:19095")
FAULT_FILE=os.environ.get("FAULT_FILE","/tmp/sidecar_fault")
WORKLOAD=os.environ.get("WORKLOAD","./bin/workload_cuda")
VISENV=os.environ.get("VISENV","CUDA_VISIBLE_DEVICES"); VISDEV=os.environ.get("VISDEV","4")
DEVICE=os.environ.get("DEVICE","6")  # device to fault-inject (string id as seen by sidecar)
EXCLUDE=set(os.environ.get("EXCLUDE","").split(","))-{""}
NODE=os.environ.get("NODE_LABEL","h100")
OUTDIR=os.environ.get("OUTDIR","artifacts/validation_round_2/h100_raw")
os.makedirs(OUTDIR, exist_ok=True)

def status():
    try:
        with urllib.request.urlopen(SIDECAR+"/v1/status",timeout=5) as r: return json.load(r)
    except Exception as e: return {"error":str(e),"devices":[]}
def readyz():
    try:
        req=urllib.request.Request(SIDECAR+"/readyz")
        with urllib.request.urlopen(req,timeout=5) as r: return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, json.load(e)
    except Exception as e:
        return 0, {"error":str(e)}
def dev(st,d):
    for x in st.get("devices",[]):
        if x["identity"]["device_id"]==d: return x
    return None
def snapshot(phase, focusdev):
    st=status(); code,rz=readyz(); d=dev(st,focusdev)
    rec={"t":round(time.time()-T0,2),"phase":phase,"readyz_code":code,"readyz_ready":rz.get("ready")}
    if d:
        rec.update({"state":d["lifecycle_state"],"reason_codes":d["lifecycle"]["reason_codes"],
                    "soft_failures":d["lifecycle"]["consecutive_soft_failures"],
                    "hard_offline":d["lifecycle"]["hard_offline"],
                    "stability":round(d["stability"]["score"],4),
                    "cap_hint":round(d["capacity"]["host_capacity_hint"],3),
                    "cap_semantics":d["capacity"]["capacity_semantics"]})
    # device-specific readyz reasons
    for dd in rz.get("details",[]):
        if dd["device_id"]==focusdev:
            rec["readyz_dev_ready"]=dd["ready"]; rec["readyz_dev_reasons"]=dd["reasons"]
    SAMPLES.append(rec); return rec

T0=time.time(); SAMPLES=[]; RESULTS={"node":NODE,"events":[]}
def ev(k,**kw): RESULTS["events"].append({"t":round(time.time()-T0,2),"kind":k,**kw}); print(f"[{round(time.time()-T0,2)}] {k} {kw}")

open(FAULT_FILE,"w").write("clear\n")
# Phase A: healthy baseline
ev("GROUND_TRUTH_baseline")
for _ in range(6): snapshot("baseline", DEVICE); time.sleep(0.5)

# Phase B: ONE transient soft failure -> expect DEGRADED, NOT OFFLINE
ev("GROUND_TRUTH_inject_one_soft_failure", device=DEVICE)
open(FAULT_FILE,"w").write(f"failsoft {DEVICE}\n")
time.sleep(2.5)  # ~1 poll cycle
snapshot("one_soft_failure", DEVICE)
open(FAULT_FILE,"w").write("clear\n")
ev("GROUND_TRUTH_clear_soft_failure")
time.sleep(3)
for _ in range(4): snapshot("after_soft_clear", DEVICE); time.sleep(0.5)

# Phase C: consecutive soft failures reaching OFFLINE threshold
ev("GROUND_TRUTH_inject_sustained_soft_failures", device=DEVICE)
open(FAULT_FILE,"w").write(f"failsoft {DEVICE}\n")
offline_seen_t=None
for i in range(16):
    r=snapshot("sustained_soft", DEVICE)
    if offline_seen_t is None and r.get("state")=="OFFLINE":
        offline_seen_t=time.time(); ev("SIDECAR_OBSERVED_offline", soft_failures=r.get("soft_failures"))
        break
    time.sleep(1.0)

# Phase D: recovery -> RECOVERING -> READY
ev("GROUND_TRUTH_clear_failure_for_recovery")
open(FAULT_FILE,"w").write("clear\n")
recovering_t=None; ready_t=None
for i in range(40):
    r=snapshot("recovery", DEVICE)
    if recovering_t is None and r.get("state")=="RECOVERING":
        recovering_t=time.time(); ev("SIDECAR_OBSERVED_recovering")
    if ready_t is None and r.get("state")in("READY","BUSY"):
        ready_t=time.time(); ev("SIDECAR_OBSERVED_back_to_ready", state=r.get("state")); break
    time.sleep(0.5)

# Phase E: drain via POST + undrain
ev("GROUND_TRUTH_drain_post", device=DEVICE)
def drain(on):
    body=json.dumps({"device":DEVICE,"on":on}).encode()
    req=urllib.request.Request(SIDECAR+"/v1/drain",data=body,method="POST",headers={"Content-Type":"application/json"})
    try:
        with urllib.request.urlopen(req,timeout=5) as r: return r.status, json.load(r)
    except urllib.error.HTTPError as e: return e.code, json.load(e)
code,resp=drain(True); ev("SIDECAR_drain_response", code=code, resp=resp)
time.sleep(3)
for _ in range(3): snapshot("drained", DEVICE); time.sleep(0.5)
# verify GET is rejected
try:
    req=urllib.request.Request(SIDECAR+f"/v1/drain?device={DEVICE}&on=false",method="GET")
    with urllib.request.urlopen(req,timeout=5) as r: get_code=r.status
except urllib.error.HTTPError as e: get_code=e.code
ev("SIDECAR_drain_GET_rejected", code=get_code, expect=405)
code,resp=drain(False); ev("SIDECAR_undrain_response", code=code, resp=resp)
time.sleep(3); 
for _ in range(3): snapshot("undrained", DEVICE); time.sleep(0.5)

RESULTS["offline_detect_after_sustained_s"]=round(offline_seen_t-T0,2) if offline_seen_t else None
RESULTS["recovering_after_clear_s"]=round(recovering_t-T0,2) if recovering_t else None
RESULTS["ready_after_clear_s"]=round(ready_t-T0,2) if ready_t else None
RESULTS["samples"]=SAMPLES
fn=os.path.join(OUTDIR,f"validate_round2_{NODE}_{int(T0)}.json")
json.dump(RESULTS,open(fn,"w"),indent=2)
print("SAVED",fn)
# print concise verification
def phase_states(p): return [s.get("state") for s in SAMPLES if s["phase"]==p]
print("\n=== VERIFICATION ===")
print("one_soft_failure states:", phase_states("one_soft_failure"), "(expect DEGRADED, NOT OFFLINE)")
print("sustained_soft reached OFFLINE:", any(s.get("state")=="OFFLINE" for s in SAMPLES if s["phase"]=="sustained_soft"))
print("recovery passed through RECOVERING:", any(s.get("state")=="RECOVERING" for s in SAMPLES if s["phase"]=="recovery"))
print("readyz failed during offline:", any(s.get("readyz_ready")==False for s in SAMPLES if s["phase"]=="sustained_soft"))
