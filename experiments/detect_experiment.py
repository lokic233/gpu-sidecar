#!/usr/bin/env python3
"""Detection-latency experiment: launch a controlled GPU workload, poll the sidecar,
and measure detection delay for workload start, BUSY transition, and stop.
Writes ground-truth + observation timestamps to artifacts."""
import json, os, subprocess, sys, time, urllib.request

SIDECAR = os.environ.get("SIDECAR", "http://localhost:19095")
DEVICE  = os.environ.get("DEVICE", "3")
WORKLOAD= os.environ.get("WORKLOAD", "./bin/workload_cuda")
VISENV  = os.environ.get("VISENV", "CUDA_VISIBLE_DEVICES")
SECS    = int(os.environ.get("SECS", "25"))
MEMGB   = os.environ.get("MEMGB", "20")
POLL_MS = 250

def get_status():
    try:
        with urllib.request.urlopen(SIDECAR + "/v1/status", timeout=4) as r:
            return json.load(r)
    except Exception as e:
        return {"error": str(e)}

def dev_view(st, dev):
    for d in st.get("devices", []):
        if d["identity"]["device_id"] == dev:
            return d
    return None

def main():
    print(f"# detect experiment device={DEVICE} workload={WORKLOAD} secs={SECS} memgb={MEMGB}")
    samples = []
    t0 = time.time()

    # baseline: confirm READY/idle
    base = dev_view(get_status(), DEVICE)
    print(f"baseline state={base['lifecycle_state']} util={base['health']['utilization_gpu_pct']}")

    # launch workload pinned to device
    env = dict(os.environ); env[VISENV] = DEVICE
    launch_t = time.time()
    # stdout/stderr -> DEVNULL to avoid pipe-buffer deadlock (workload heartbeats not needed here)
    proc = subprocess.Popen([WORKLOAD, "sustained", str(SECS), str(MEMGB)],
                            env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    print(f"GROUND_TRUTH workload_launch t={launch_t-t0:.3f}s pid={proc.pid}")

    detect_busy_t = None
    detect_procs_t = None
    detect_mem_t = None
    stop_detected_t = None
    stop_t = None

    # poll loop
    while True:
        now = time.time()
        st = get_status()
        d = dev_view(st, DEVICE)
        if d:
            h = d["health"]
            util = h["utilization_gpu_pct"]["value"] if h["utilization_gpu_pct"]["supported"] else 0
            memused = h["mem_used_bytes"]["value"] if h["mem_used_bytes"]["supported"] else 0
            procs = h["compute_proc_count"]["value"] if h["compute_proc_count"]["supported"] else 0
            state = d["lifecycle_state"]
            samples.append({"t": round(now-t0,3), "state": state, "util": util,
                            "mem_used_gb": round(memused/1e9,2), "procs": procs,
                            "stability": round(d["stability"]["score"],4),
                            "effcap": round(d["effective_capacity"],4)})
            if detect_procs_t is None and procs > (base["health"]["compute_proc_count"]["value"]):
                detect_procs_t = now
                print(f"DETECT worker_start(procs>0) t={now-t0:.3f}s delay={now-launch_t:.3f}s")
            if detect_mem_t is None and memused > base["health"]["mem_used_bytes"]["value"] + 1e9:
                detect_mem_t = now
                print(f"DETECT mem_alloc t={now-t0:.3f}s delay={now-launch_t:.3f}s")
            if detect_busy_t is None and state in ("BUSY","DEGRADED"):
                detect_busy_t = now
                print(f"DETECT state->{state} t={now-t0:.3f}s delay={now-launch_t:.3f}s")

        # detect process exit (ground truth stop)
        if proc.poll() is not None and stop_t is None:
            stop_t = time.time()
            print(f"GROUND_TRUTH workload_exit t={stop_t-t0:.3f}s code={proc.returncode}")
        # after stop, detect procs back to baseline
        if stop_t is not None and stop_detected_t is None and d:
            procs = d["health"]["compute_proc_count"]["value"] if d["health"]["compute_proc_count"]["supported"] else 0
            if procs <= base["health"]["compute_proc_count"]["value"]:
                stop_detected_t = now
                print(f"DETECT worker_stop t={now-t0:.3f}s delay={now-stop_t:.3f}s")
                break
        if now - t0 > SECS + 20:
            print("timeout waiting for stop detection")
            break
        time.sleep(POLL_MS/1000.0)

    result = {
        "device": DEVICE, "workload": WORKLOAD,
        "launch_delay_procs_s": round(detect_procs_t-launch_t,3) if detect_procs_t else None,
        "launch_delay_mem_s": round(detect_mem_t-launch_t,3) if detect_mem_t else None,
        "launch_delay_busy_s": round(detect_busy_t-launch_t,3) if detect_busy_t else None,
        "stop_delay_s": round(stop_detected_t-stop_t,3) if (stop_detected_t and stop_t) else None,
        "poll_ms": POLL_MS, "samples": samples,
    }
    outdir = sys.argv[1] if len(sys.argv)>1 else "artifacts/time_series"
    os.makedirs(outdir, exist_ok=True)
    fn = os.path.join(outdir, f"detect_{os.path.basename(WORKLOAD)}_dev{DEVICE}_{int(t0)}.json")
    json.dump(result, open(fn,"w"), indent=2)
    print(f"\nSAVED {fn}")
    print(f"SUMMARY procs_detect={result['launch_delay_procs_s']}s mem_detect={result['launch_delay_mem_s']}s busy_detect={result['launch_delay_busy_s']}s stop_detect={result['stop_delay_s']}s")

if __name__ == "__main__":
    main()
