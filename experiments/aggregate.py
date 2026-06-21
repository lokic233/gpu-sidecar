#!/usr/bin/env python3
"""Aggregate raw experiment JSON into the required CSV artifacts."""
import json, glob, os, statistics as st, csv
ART="artifacts"
def med(a): return round(st.median(a),3) if a else ""
def p95(a):
    if not a: return ""
    a=sorted(a); k=0.95*(len(a)-1); lo=int(k)
    return round(a[lo] if lo>=len(a)-1 else a[lo]+(k-lo)*(a[lo+1]-a[lo]),3)

# detection_latency_results.csv
rows=[]
for node,patt,wl in [("h100","artifacts/h100_raw/detect_*.json","cuda"),
                     ("h100","artifacts/h100_raw/autodetect_*.json","cuda"),
                     ("mi350x","artifacts/mi350x_raw/autodetect_*.json","hip")]:
    for f in glob.glob(patt):
        try:
            d=json.load(open(f))
        except Exception:
            continue
        # only detection-type records (have these keys)
        if not any(k in d for k in ("start_delay_s","launch_delay_procs_s","busy_delay_s","launch_delay_busy_s")):
            continue
        rows.append({"node":node,"workload":wl,"file":os.path.basename(f),
            "start_s":d.get("start_delay_s") or d.get("launch_delay_procs_s"),
            "busy_s":d.get("busy_delay_s") or d.get("launch_delay_busy_s"),
            "stop_s":d.get("stop_delay_s")})
with open(f"{ART}/detection_latency_results.csv","w",newline="") as fh:
    w=csv.DictWriter(fh,fieldnames=["node","workload","file","start_s","busy_s","stop_s"]); w.writeheader(); w.writerows(rows)

# summary by node
def col(node,k):
    return [r[k] for r in rows if r["node"]==node and r[k] is not None]
print("=== detection latency summary ===")
for node in ["h100","mi350x"]:
    for k in ["start_s","busy_s","stop_s"]:
        v=col(node,k)
        print(f"{node:7s} {k:8s} n={len(v)} median={med(v)} p95={p95(v)} min={min(v) if v else ''} max={max(v) if v else ''}")

# overhead_results.csv
orows=[]
for node,patt in [("h100","artifacts/h100_raw/overhead_*.json"),("mi350x","artifacts/mi350x_raw/overhead_*.json")]:
    for f in glob.glob(patt):
        d=json.load(open(f))
        orows.append({"node":node,"rss_mb":d["rss_mb"],"cpu_pct_one_core":d["cpu_pct_of_one_core"],
            "n_dev":d["probe_latency_ms"]["n_dev"],
            "probe_p50_ms":d["probe_latency_ms"]["p50"],"probe_p95_ms":d["probe_latency_ms"]["p95"],
            "api_status_p50_ms":d["api_status_ms"]["p50"],"api_status_p95_ms":d["api_status_ms"]["p95"],
            "api_metrics_p50_ms":d["api_metrics_ms"]["p50"],"api_metrics_p95_ms":d["api_metrics_ms"]["p95"]})
with open(f"{ART}/overhead_results.csv","w",newline="") as fh:
    if orows:
        w=csv.DictWriter(fh,fieldnames=list(orows[0].keys())); w.writeheader(); w.writerows(orows)
print("\n=== overhead summary ===")
for r in orows: print(r)
print("\nWROTE detection_latency_results.csv, overhead_results.csv")
