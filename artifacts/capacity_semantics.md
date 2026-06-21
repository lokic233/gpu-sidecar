# Capacity Semantics — host hint vs serving capacity

The round-1 `effective_capacity` field implied a measurement of how much LLM-serving work a backend
could accept. A pure host sidecar CANNOT measure that. The field is renamed and re-scoped.

## What changed
| Round 1 | Round 2 |
|---|---|
| `effective_capacity` (float, "serving-capacity estimate") | `capacity.host_capacity_hint` (float, explicitly heuristic) |
| no semantics label | `capacity_semantics = "heuristic_host_derived"` |
| opaque formula | `capacity.components` map exposed |
| (none) | `runtime_serving_capacity_supported = false` + optional plugin struct |
| metric `gpu_effective_capacity` | metric `gpu_host_capacity_hint` |

## host_capacity_hint (heuristic, host-derived)
```
host_capacity_hint = clamp01( free_memory_ratio × utilization_headroom × stability_score )
components = { free_memory_ratio, utilization_headroom, stability_score }
```
This is a coarse admission-risk hint. It is **NOT** a number of requests, tokens, or sequences a
backend can accept. A router must treat it as a soft prior, not a quota.

## Runtime serving capacity (NOT observable from host)
`RuntimeServingCapacity` (optional plugin) would carry queue depth, KV-cache occupancy/fragmentation,
loaded model, max admissible batch, prefill/decode capacity, TTFT, TPOT, active sequences, goodput.
None are reliably observable from `nvidia-smi`/`rocm-smi`. `runtime_serving_capacity_supported`
is `false` and `runtime_serving_capacity` is `null` until a runtime adapter (vLLM/TGI/SGLang
metrics) is connected. The plugin interface is defined but no runtime adapter is implemented this round.

## Real-hardware evidence
`/v1/status` device: `capacity.capacity_semantics = "heuristic_host_derived"`,
`runtime_serving_capacity_supported = false`, components exposed
(e.g. free_memory_ratio 0.995, utilization_headroom 1.0, stability_score 1.0 → hint 0.994).
