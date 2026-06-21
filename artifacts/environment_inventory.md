# Environment Inventory

Captured during Phase 0 discovery. Secrets/credentials removed. All commands run as
unprivileged user `dengcchi` (no root). Two real GPU hosts reached via the Navi CLI mesh.

## Node A — NVIDIA H100 (`cli:devgpu014`)

| Field | Value |
|---|---|
| Hostname | `devgpu014.eag3.facebook.com` |
| OS | CentOS Stream 9 |
| Kernel | `6.13.2-0_fbk12_0_g0b66b3635210` |
| GPU model / count | NVIDIA H100 ×8 (97871 MiB each) |
| Driver | 580.82.07 |
| CUDA toolkits | 12.0, 12.4, 12.8, 12.9, 13.0 (`/usr/local/cuda*/bin/nvcc`) |
| boot_id | `6033736e-8996-40fa-a500-ca438a2eee40` |
| User | `dengcchi` (unprivileged) |

### Available tooling
- `nvidia-smi` ✅ (full `--query-gpu` field set incl. ECC, power, clocks)
- `dcgmi` ✅ (8 GPUs discovered, health subsystem available)
- `dmesg` ✅ readable without root → XID scraping possible
- `nvcc` ✅ (used to build the lightweight compute probe, `-arch=sm_90`)
- Go 1.26.2 ✅ ; `gh` ✅ authed (lokic233) ; git 2.53
- PyTorch ❌ (no `torch`) ; cupy ❌ ; numba ❌ → **no Python GPU stack**, probes use native CUDA

### GPU occupancy at discovery (shared host — DO NOT DISTURB busy GPUs)
```
idx  mem.used   util   note
0    106 MiB    0%     free
1    120 MiB    0%     free
2    77105 MiB  0%     BUSY (other user pid 2753095) — avoid
3    4 MiB      0%     free  <- sidecar/workload target
4    4 MiB      0%     free  <- sidecar/workload target
5    88229 MiB  0%     BUSY (other user pid 3850083) — avoid
6    4 MiB      0%     free
7    4 MiB      0%     free
```
`nvidia-smi --query-gpu=...` sample (gpu0):
```
0, GPU-89bbd3dc-..., NVIDIA H100, 580.82.07, P0, 106, 97251, 97871, 0, 0, 32, 67.55, 500.00, 345, 1593, 0
(index, uuid, name, driver, pstate, mem.used, mem.free, mem.total, util.gpu, util.mem, temp, power.draw, power.limit, sm_clk, mem_clk, ecc.uncorr.total)
```

## Node B — AMD MI350X (`cli:devgpu499`)

| Field | Value |
|---|---|
| Hostname | `devgpu499.ldc2.facebook.com` |
| OS | CentOS Stream 9 |
| Kernel | `6.9.0-0_fbk10_brcmrdma17_154_g76f7bfd798a1` |
| GPU model / count | AMD Instinct MI350X (gfx950, `0x75a0`) ×8 (288 GB / 309220868096 B each) |
| ROCm | 7.0 (HIP 7.0.51831), `hipcc` AMD clang 20 |
| boot_id | `5f3dc25c-ac40-4bc3-91aa-9f1a9d3e519b` |
| User | `dengcchi` (unprivileged, **NOT in render/video groups**) |

### Available tooling
- `rocm-smi` ✅ **primary AMD source** — full JSON: temp(junction/memory), use%, power, clocks(sclk/mclk/fclk/socclk), VRAM bytes, VRAM%, PIDs, RAS info
- `amd-smi` ⚠️ **BLOCKED**: `RuntimeError: User is missing the following required groups: render, video`. Only `amd-smi static` partially works. → adapter must NOT depend on amd-smi.
- `dmesg` ✅ readable → AMD GPU error scraping possible
- `hipcc` ✅ (`--offload-arch=gfx950`, used to build the HIP compute probe)
- Go 1.26.2 ✅ ; `gh` ✅ authed (lokic233) ; git
- PyTorch ❌ ; amdsmi python ❌ → probes use native HIP

### GPU occupancy at discovery
```
card  VRAM used        use%  note
0     282454867968 B   0%    BUSY (other workload, 91% VRAM) — avoid
1     809197568 B      0%    BUSY (vLLM EngineCore pid 833684) — avoid
2..7  ~809 MB          0%    free  <- workload targets
```
`rocm-smi --json` sample (card0): junction temp 63C, mem temp 52C, sclk 1393Mhz, power 272W, VRAM 91%.
`rocm-smi --showrasinfo`: UMC block ENABLED, 0 correctable / 0 uncorrectable.

## Mesh / inter-node reachability
- Direct SSH between nodes is 2FA-locked (not usable for automation).
- **IPv6 TCP on high ports works node↔node** — verified: MI350X connected to a listener on
  H100 `[2401:db00:33c:2c1c:face:0:266:0]:9998` and received `PONG-from-014`.
- The collector therefore polls each sidecar's HTTP port over IPv6. The Navi CLI mesh is also
  used as a control channel (the collector can be driven from the workspace via either sidecar).
- H100 routable IPv6: `2401:db00:33c:2c1c:face:0:266:0`
- MI350X routable IPv6: `2401:db00:272c:590b:face:0:133:0`

## Key engineering implications
1. **Go, stdlib-only**: one static binary per node. No root, no pip, survives the missing Python GPU stack.
2. **Vendor asymmetry is real**: NVIDIA exposes ECC/XID; AMD's richest tool (`amd-smi`) is permission-blocked,
   so the AMD adapter is built on `rocm-smi --json` + `--showrasinfo` + `dmesg`. Missing fields are marked, not faked.
3. **Shared hosts**: both nodes have other users' live jobs. Workloads/probes pin to known-free GPUs
   (H100: 3,4,6,7 / MI350X: 2-7) and never touch busy devices. Conservative thresholds on MI350X (prior expensive failures).
