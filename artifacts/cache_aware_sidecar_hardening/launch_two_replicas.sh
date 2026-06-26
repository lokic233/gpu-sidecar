#!/bin/bash
# Launch TWO genuinely independent vLLM replicas on H100 for a VALID equal-capability experiment:
# independent process, independent GPU, independent port, independent KV cache, independent scheduler.
# Identical model/dtype/TP/max-len/KV-config/version/flags.
set -e
CUBLAS=~/vllm-env/lib/python3.12/site-packages/nvidia/cu13/lib/libcublas.so.13
CUBLASLT=~/vllm-env/lib/python3.12/site-packages/nvidia/cu13/lib/libcublasLt.so.13
COMMON="--model Qwen/Qwen2.5-0.5B-Instruct --host 127.0.0.1 --max-model-len 4096 \
  --gpu-memory-utilization 0.30 --enforce-eager --no-enable-log-requests"

launch_replica () {
  local gpu=$1 port=$2 zmq=$3 log=$4
  PATH="$HOME/vllm-env/bin:$PATH" LD_PRELOAD="$CUBLAS:$CUBLASLT" VLLM_ATTENTION_BACKEND=FLASH_ATTN \
    HF_HUB_OFFLINE=1 CUDA_VISIBLE_DEVICES=$gpu \
    ~/vllm-env/bin/python -m vllm.entrypoints.openai.api_server $COMMON --port $port \
    --kv-events-config "{\"enable_kv_cache_events\": true, \"publisher\": \"zmq\", \"endpoint\": \"tcp://*:$zmq\", \"topic\": \"kv@h100gpu$gpu\"}" \
    > "$log" 2>&1 &
  echo "replica gpu=$gpu port=$port zmq=$zmq pid=$!"
}

mkdir -p ~/gpu-sidecar/artifacts/cache_aware_sidecar_hardening/replica_logs
launch_replica 6 8006 5560 ~/gpu-sidecar/artifacts/cache_aware_sidecar_hardening/replica_logs/vllm_A_gpu6.log
launch_replica 7 8007 5561 ~/gpu-sidecar/artifacts/cache_aware_sidecar_hardening/replica_logs/vllm_B_gpu7.log
echo "two independent replicas launching (GPU6:8006, GPU7:8007)"
