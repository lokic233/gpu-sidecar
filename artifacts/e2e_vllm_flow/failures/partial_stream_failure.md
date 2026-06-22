# Partial Stream Failure (post-first-token)

Semantics (implemented + unit-tested): once the FIRST response byte has reached the client, the
router does NOT transparently reroute. If the selected backend fails mid-stream:
- Stream is terminated cleanly.
- Event recorded: PARTIAL_STREAM_FAILED with emitted events + bytes + failure reason.
- No restart of generation on another backend (first implementation).

Distinguished from CANCELLED: a client/router context-cancellation mid-stream is classified
CLIENT_CANCELLED/UPSTREAM_CANCELLED, NOT PARTIAL_STREAM_FAILED. A genuine upstream (vLLM) read error
mid-stream with the client still connected is PARTIAL_STREAM_FAILED.

Unit coverage: dataplane proxy + router relay distinguish ctx-cancel from upstream error on both
pre-first-byte (retryable / cancelled) and post-first-byte (terminal) paths.
