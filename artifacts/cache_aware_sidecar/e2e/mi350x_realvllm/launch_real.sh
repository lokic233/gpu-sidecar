#!/bin/bash
export HF_HUB_OFFLINE=1
export HIP_VISIBLE_DEVICES=4
exec ~/edmm_rocm_env/bin/python -m vllm.entrypoints.openai.api_server \
  --model Qwen/Qwen2.5-0.5B-Instruct --port 8001 --host 127.0.0.1 \
  --max-model-len 2048 --enforce-eager \
  --kv-cache-memory-bytes 8589934592 \
  --kv-events-config '{"enable_kv_cache_events": true, "publisher": "zmq", "endpoint": "tcp://*:5557", "topic": "kv@mi350x"}'
