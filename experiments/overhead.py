#!/usr/bin/env python3
"""Measure sidecar overhead: CPU%, RSS, API latency (status & metrics), poll latency.
Reads /proc for the sidecar pid; times endpoints over N samples."""
import json, os, sys, time, urllib.request
SIDECAR=os.environ.get("SIDECAR","http://localhost:19095"); N=int(os.environ.get("N","60"))
def find_pid():
    for pid in os.listdir("/proc"):
        if not pid.isdigit(): continue
        try:
            cl=open(f"/proc/{pid}/cmdline","rb").read().replace(b"\x00",b" ").decode()
            if "bin/sidecar" in cl and "-listen" in cl: return int(pid)
        except Exception: pass
    return None
def rss_kb(pid):
    for line in open(f"/proc/{pid}/status"):
        if line.startswith("VmRSS:"): return int(line.split()[1])
    return 0
def cpu_ticks(pid):
    f=open(f"/proc/{pid}/stat").read().split()
    return int(f[13])+int(f[14])  # utime+stime
def total_ticks():
    f=open("/proc/stat").readline().split()[1:]
    return sum(int(x) for x in f)
def timed(url):
    t=time.time()
    with urllib.request.urlopen(url,timeout=5) as r: r.read()
    return (time.time()-t)*1000.0
def pctl(a,p):
    if not a: return 0
    a=sorted(a); k=(p/100)*(len(a)-1); lo=int(k); 
    return a[lo] if lo>=len(a)-1 else a[lo]+(k-lo)*(a[lo+1]-a[lo])
def main():
    pid=find_pid()
    if not pid: print("sidecar pid not found"); sys.exit(1)
    print(f"sidecar pid={pid}")
    HZ=os.sysconf("SC_CLK_TCK")
    c0,t0=cpu_ticks(pid),total_ticks(); time.sleep(2); c1,t1=cpu_ticks(pid),total_ticks()
    ncpu=os.cpu_count()
    cpu_pct=100.0*(c1-c0)/(t1-t0)*ncpu  # % of one core
    status_l=[timed(SIDECAR+"/v1/status") for _ in range(N)]
    metrics_l=[timed(SIDECAR+"/metrics") for _ in range(N)]
    health_l=[timed(SIDECAR+"/healthz") for _ in range(N)]
    # poll latency from status (probe_latency_ms of devices)
    st=json.load(urllib.request.urlopen(SIDECAR+"/v1/status",timeout=5))
    probe_l=[d["health"]["probe_latency_ms"] for d in st["devices"]]
    res={"pid":pid,"rss_mb":round(rss_kb(pid)/1024,2),"cpu_pct_of_one_core":round(cpu_pct,3),
         "ncpu":ncpu,"samples":N,
         "api_status_ms":{"p50":round(pctl(status_l,50),3),"p95":round(pctl(status_l,95),3)},
         "api_metrics_ms":{"p50":round(pctl(metrics_l,50),3),"p95":round(pctl(metrics_l,95),3)},
         "api_healthz_ms":{"p50":round(pctl(health_l,50),3),"p95":round(pctl(health_l,95),3)},
         "probe_latency_ms":{"p50":round(pctl(probe_l,50),3),"p95":round(pctl(probe_l,95),3),"n_dev":len(probe_l)}}
    outdir=sys.argv[1] if len(sys.argv)>1 else "artifacts"; os.makedirs(outdir,exist_ok=True)
    fn=os.path.join(outdir,f"overhead_{os.uname().nodename.split('.')[0]}_{int(time.time())}.json")
    json.dump(res,open(fn,"w"),indent=2); print(json.dumps(res,indent=2)); print("SAVED",fn)
if __name__=="__main__": main()
