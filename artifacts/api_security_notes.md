# API Security Notes

This sidecar is host-truth infrastructure for an internal mesh. This round hardened the obvious
correctness/security footgun (state change via GET) but does NOT claim production security.

## What was fixed
- **Drain is no longer a GET mutation.** `/v1/drain` accepts **POST/PUT only**; GET returns
  `405 Method Not Allowed` with an `Allow: POST, PUT` header. Fields `device` and `on` are required
  and validated (JSON body or form). The operation is **idempotent** (`changed` flag) and records a
  lifecycle event with previous/new draining state and the request source (RemoteAddr).

## What is NOT yet provided (do NOT claim production security without these)
- **Authentication** — no caller identity is verified.
- **Authorization** — no per-caller permission checks on mutations.
- **Transport security** — HTTP is plaintext; no TLS/mTLS.
- **Auditability** — drain events are recorded in-memory only (bounded ring), not to durable audit log.

## Deployment guidance (current safe posture)
- Bind to **localhost or a trusted mesh interface only**. The `-listen` flag is configurable; default
  `[::]:19095` exposes all interfaces and SHOULD be restricted in shared environments.
- Read endpoints (`/healthz`, `/readyz`, `/v1/status`, `/v1/history`, `/v1/events`, `/metrics`) are
  non-mutating. The only mutation is `/v1/drain`.
- For production: front with mTLS + an authz proxy, or add native mTLS + token authz and a durable
  audit sink before exposing the mutation endpoint beyond a trusted operator plane.
