# Deployment Log

Both sidecars deployed and validated on real hardware via the Navi CLI mesh. No root used.

## H100 node (devgpu014.eag3.facebook.com)
```bash
# build (Go 1.26, no deps)
cd ~/gpu-sidecar && go build -o bin/sidecar ./cmd/sidecar && go build -o bin/collector ./cmd/collector
# launch (auto-detected nvidia adapter; pinned to free GPUs 3,4,6,7 — GPUs 2,5 held by other users)
GPU_SIDECAR_FAULT_FILE=/tmp/sidecar_fault ./bin/sidecar -listen "[::]:19095" -devices 3,4,6,7 -poll 2s
```
Startup log:
```
sidecar 0.1.0 starting on devgpu014.eag3.facebook.com | adapter=nvidia (nvidia-smi) | boot=6033736e-...
monitoring 4 device(s)
HTTP listening on [::]:19095
```
Verified: /healthz 200, /readyz {devices:4, ready}, /v1/status returns 4 H100s with real telemetry
(driver 580.82.07, CUDA 13.0, temps 28-36C, ECC=0, 102GB free each).

## MI350X node (devgpu499.ldc2.facebook.com)
```bash
git clone https://github.com/lokic233/gpu-sidecar && cd gpu-sidecar
go build -o bin/sidecar ./cmd/sidecar
# launch (auto-detected amd adapter; GPUs 2-7, GPUs 0,1 held by other users incl. a vLLM job)
GPU_SIDECAR_FAULT_FILE=/tmp/sidecar_fault ./bin/sidecar -listen "[::]:19095" -devices 2,3,4,5,6,7 -poll 2s
```
Startup log:
```
sidecar 0.1.0 starting on devgpu499.ldc2.facebook.com | adapter=amd (rocm-smi) | boot=5f3dc25c-...
monitoring 6 device(s)
HTTP listening on [::]:19095
```
Verified: /healthz 200, /readyz {devices:6, ready}, /v1/status returns 6 MI350X with real telemetry
(driver 6.16.6, ROCm-SMI, junction temps 54-62C, power 247-268W, RAS=0, 308GB free each).
amd-smi BLOCKED (render/video groups) → adapter uses rocm-smi. power_limit marked unsupported.

## Mesh collector (run from H100, polls both over IPv6)
```bash
./bin/collector -once -format table \
  -sidecars "h100=http://[::1]:19095,mi350x=http://[2401:db00:272c:590b:face:0:133:0]:19095"
```
Output: one normalized table of all 10 GPUs (4 nvidia + 6 amd). See cross_vendor_matrix.csv + mesh_raw/.

## Process-supervision note
The CLI tears down child processes when a foreground shell exits; sidecars were launched as
tracked background processes. Avoid broad `pkill -f 'workload'` — it matches sibling commands.
Stop a sidecar by port: `fuser -k <port>/tcp`.
