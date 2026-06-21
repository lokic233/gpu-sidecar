# Readiness Contract Results (Round 3)

## Host-level /readyz (control-plane readiness)
Returns explicit aggregate fields; 200 iff control-plane ready, else 503:
```json
{ "control_plane_ready": true, "any_device_ready": true, "all_devices_ready": false,
  "ready_device_count": 7, "total_device_count": 8, "details": [ {device-level...} ] }
```
- `control_plane_ready`: collected + not stalled + ≥1 trustworthy device. NOT proof every GPU serves.
- `any_device_ready` / `all_devices_ready` make partial multi-GPU failure unambiguous.
- Backward-compat aliases retained: `ready`, `ready_devices`, `total_devices`.

## Per-device /readyz?device=N
- **200** device satisfies readiness; **503** it does not (with `reasons[]`); **404** unmanaged device.

## Tests
- API (internal/api/readiness_test.go): AllDevicesReady, SomeDevicesReady, NoDevicesReady,
  PerDevice_Ready/NotReady/Invalid, OneInaccessibleDevice.
- Supervisor (internal/engine/readiness_agg_test.go): FirstCollectionNotDone, AllReady, SomeReady,
  CollectorStalled, PerDeviceStale, PerDeviceUnmanaged.

## Real-hardware evidence
- H100: host /readyz `4/4 ready, all flags true`; `?device=4`→200; `?device=99`→404. (`h100_smoke/`)
- MI350X: host /readyz `6/6 ready`; `?device=5`→200; `?device=99`→404; GET /v1/drain→405. (`mi350x_smoke/`)
- Partial: API test `SomeDevicesReady` proves `all_devices_ready=false` with `control_plane_ready=true`
  when one device is down.
