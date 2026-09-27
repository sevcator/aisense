# Read-Only Discovery Diagnosis

Run from the application directory:

```
go run ./cmd/discoverydiag -log 2026-09-17.log
```

The command decodes JSON directly, never calls `config.Load`, and never writes
configuration, usage, or logs. Output is counts, settings, file metadata and
allowlisted event/error summaries. Log reading is limited to the last 32 MiB;
session counts are only meaningful when the session start is in that window.
Raw URLs, keys, headers, response bodies and error messages are never printed.

Optional `-probes 6` performs bounded catalog GETs against at most six distinct
enabled imported hosts (first configured credential only). Each host has a
10-second total budget, at most one auth-style retry, and a 4 MiB body limit.
Redirects are refused. Existing upstream TLS policy is respected. Proxy,
OAuth and passthrough endpoints are skipped rather than bypassing their policy.
No inference or capability POST requests are made. A successful catalog does
not verify inference, every credential, or other endpoints.
