#!/bin/bash
# One-command H100 stack: vLLM + sidecar + collector + router. Assumes ~/vllm-env + model cached.
set -e
cd "$(dirname "$0")/.."
go build -o bin/sidecar ./cmd/sidecar; go build -o bin/router ./cmd/router; go build -o bin/trajcollector ./cmd/trajcollector
CUBLAS=~/vllm-env/lib/python3.12/site-packages/nvidia/cu13/lib/libcublas.so.13
CUBLASLT=~/vllm-env/lib/python3.12/site-packages/nvidia/cu13/lib/libcublasLt.so.13
PATH="$HOME/vllm-env/bin:$PATH" LD_PRELOAD="$CUBLAS:$CUBLASLT" VLLM_ATTENTION_BACKEND=FLASH_ATTN HF_HUB_OFFLINE=1 CUDA_VISIBLE_DEVICES=3 \
  ~/vllm-env/bin/python -m vllm.entrypoints.openai.api_server --model Qwen/Qwen2.5-0.5B-Instruct --port 8000 --host 127.0.0.1 --max-model-len 4096 --gpu-memory-utilization 0.30 --enforce-eager --no-enable-log-requests &
sleep 60
./bin/trajcollector -listen "[::]:29100" -out artifacts/e2e_vllm_flow/router/joined_trajectories/events.jsonl &
GPU_SIDECAR_FAULT_FILE=/tmp/sidecar_fault ./bin/sidecar -listen "[::]:19095" -devices 3 -poll 2s -data-plane -vllm-url http://127.0.0.1:8000 -backend-id h100-gpu3 -dp-device 3 -max-queued 256 -max-inflight 64 -collector-url http://127.0.0.1:29100/v1/events &
sleep 5
./bin/router -listen 127.0.0.1:19090 -backends '[{"id":"h100-gpu3","vendor":"nvidia","sidecar_url":"http://127.0.0.1:19095","snapshot_url":"http://127.0.0.1:19095"}]' -policy least_queued -collector-url http://127.0.0.1:29100/v1/events &
echo "H100 stack up: router on 127.0.0.1:19090"
