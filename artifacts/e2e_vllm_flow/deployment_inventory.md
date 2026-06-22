# Deployment Inventory

## H100 host (devgpu014.eag3.facebook.com) — Router + Collector + Backend
- GPU: NVIDIA H100 (GPU 3 used), driver 580.82.07, CUDA 13.0
- vLLM 0.23.0 (PyPI, CUDA wheel), torch 2.11.0+cu130, Python 3.12 venv ~/vllm-env
- model: Qwen/Qwen2.5-0.5B-Instruct (rev 7ae557604adf67be50417f59c2c2f167def9a775), bf16, no quant, TP=1
- vLLM cmdline: see h100/vllm_launch.txt (LD_PRELOAD cublas + ninja + --enforce-eager + FLASH_ATTN)
- endpoints: vLLM 127.0.0.1:8000 | sidecar [::]:19095 | router 127.0.0.1:19090/19093 | collector [::]:29100
- mesh IPv6: 2401:db00:33c:2c1c:face:0:266:0

## MI350X host (devgpu499.ldc2.facebook.com) — Backend
- GPU: AMD Instinct MI350X (gfx950, HIP dev 2 used), ROCm 7.0, driver 6.16.6
- torch 2.10.0+rocm7.0 (rocm6.4 lacked gfx950 kernels), Python 3.12 venv ~/vllm-env
- model: Qwen/Qwen2.5-0.5B-Instruct (same rev; weights transferred from H100 over mesh, sha256-verified)
- runtime: minimal OpenAI-compatible server on real gfx950 (vLLM wheel is CUDA-only; see vllm_runtime_contract.md)
- endpoints: runtime 127.0.0.1:8000 | sidecar [::]:19095
- mesh IPv6: 2401:db00:272c:590b:face:0:133:0

## Network
- IPv6 mesh between hosts; high ports reachable host<->host (validated round 1 + this round).
- Mesh caps ~1.2MB per connection: model weights transferred via checksum-verified 1MB chunking.

## Software provenance
- Go 1.26.2 on both nodes; sidecar/router/collector are single static Go binaries.
- gh authed lokic233; repo github.com/lokic233/gpu-sidecar.
