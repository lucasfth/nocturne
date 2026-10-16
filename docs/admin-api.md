# Admin API (port 8702, tailnet only)

All routes need `Authorization: Bearer <admin secret>` except `GET /admin`, static assets, and the separately signature-authenticated Lightning webhook.
Errors: `{"detail": "..."}` with 400/401/403/404/409.

## GET /admin/summary
Existing dashboard payload (earned, wallet, energy, pricing, fx, queue, days, keys, invoices).
`workers[]` now lists **every registered machine**, connected or not:

```json
{"worker_id":"mac-example","online":true,"enabled":true,"eligible":true,"heartbeat_age_s":3.1,
 "battery_pct":88,"on_ac":true,"thermal_state":"nominal","user_active":false,"tokens_per_sec":26,
 "busy":1,"capacity":2,"metrics":{"mem_pressure_pct":31,"vm_pressure_level":1,"mem_total_gb":16,
 "swap_used_mb":0,"load1":1.3,"cpu_count":10,"cpu_pct":13.4,"gpu_pct":7},
 "mac":{"owner":"Example","agent":null,"model":"MacBook Air M5 16 GB","user":"example",
        "host":"mac-example.local","ip":"192.0.2.10","jump":"operator@gateway.example.invalid","admin_user":"nocturne-admin"}}
```
Offline machines: `online:false`, numeric fields `null`, `metrics:{}`, `busy:0`.

## GET /admin/machines
`[{"worker_id","enabled","added","meta":{owner,agent,model,user,host,ip,jump,admin_user},"tunnel_key_fingerprint":"SHA256:..."|null}]`

## POST /admin/machines  -> 201
```json
{"worker_id":"mac-example","secret_sha256":"<64 hex>","tunnel_pubkey":"ssh-ed25519 AAAA...",
 "meta":{"owner":"Example","model":"MacBook Air M5 16 GB","user":"example","host":"mac-example.local","ip":"192.0.2.20"}}
```
`worker_id`: `^[a-z0-9][a-z0-9-]{0,31}$`. `secret_sha256` and `tunnel_pubkey` come from the
`DIGEST <id> <hex>` and `PUBKEY <id> ssh-ed25519 ...` lines printed by `worker/bootstrap-remote.sh`.
`tunnel_pubkey` optional (ed25519 only). 409 if the id exists.

## PATCH /admin/machines/{id}
Any of `{"enabled":bool,"meta":{...partial...},"secret_sha256":"...","tunnel_pubkey":"..."}`.
Meta values: string or null (null clears). Disabling disconnects the worker immediately.

## DELETE /admin/machines/{id} -> {"deleted": id}
Removes auth and tunnel key; the Mac can no longer connect.

## GET /admin/ledger/tail
Last ledger events.

## Configuration (Python runtime)

Supported by both Python and Go (the heading anchor is retained for existing links).

`GET /admin/config` and `PATCH /admin/config` require the same admin Bearer auth
and trusted tailnet identity checks as machine management. Responses are
`Cache-Control: no-store`; `runtime` is `python` or `go`. No frontend
configuration screen or Vite `/admin/config` proxy is wired yet.

GET example (opaque revision varies):

```json
{"schema_version":1,"runtime":"python","revision":"opaque-revision",
 "fields":{"service_name":{"type":"string","apply":"live","minimum":null,"maximum":null,"default":"inference"},
           "public_limit_concurrency":{"type":"integer","apply":"restart","minimum":1,"maximum":4096,"default":64}},
 "values":{"service_name":"nocturne","public_limit_concurrency":64},
 "effective":{"service_name":"nocturne","public_limit_concurrency":64},
 "restart_required":[]}
```

PATCH must have `Content-Type: application/json`, body <=4096 bytes, no duplicate
object keys, and exactly `revision` plus a nonempty `changes` object:

```json
{"revision":"opaque-revision","changes":{"service_name":"My inference","public_limit_concurrency":128}}
```

Returns the same snapshot shape, a fresh revision, `restart_required` containing
`public_limit_concurrency`, saved value 128 and the unchanged effective value 64.
`service_name` applies live after successful persistence. Its string must be
printable, length 1-80, excluding `<`, `>`, `&`, single and double quotes.
Integer settings reject booleans, null, floats and values outside metadata bounds.
Unknown/secret/path/command/backend/access-control fields are rejected, not echoed.

Errors: 400 invalid body/value, 401 invalid/missing auth, 403 unauthorized tailnet
or cross-origin access, 409 stale revision/external file edit, 413 oversized body,
415 wrong media type, 503 persistence failure. Errors never echo submitted values.
Clients must reload after conflict; external file edits require operator restart.
Use same-origin requests with Bearer auth (never cookies or query tokens).
Cross-site/same-site Fetch Metadata is rejected; Origin, if present, must be
http(s) and match Host. Trusted proxies must preserve Host and not expose this
listener publicly. No CORS enablement or automatic restart is provided.

Atomic replacement writes only the typed nonsecret overlay at
`data/settings/config.json` (0600), inside the dedicated private `data/settings`
directory (0700), with file and parent-directory fsync. The root `config.json`
is immutable: secrets, backend selection, access control and unknown extensions
are never copied into or changed by the overlay. Both runtimes load and validate
the overlay before listener construction, and reject unknown keys, invalid types,
unsafe file modes and symlink state. An absent overlay preserves existing installs.
The service retains its data-only `ReadWritePaths`; executable roots stay read-only.
An in-process lock serializes edits; optimistic revision checking prevents lost
updates. One coordinator process owns the file. This is not a multiprocess config
store. A post-rename fsync failure returns 503 and blocks further API access until
restart because disk durability is uncertain; running live values are not updated.

Go additionally rejects non-UTF-8 JSON bytes and unpaired UTF-16 surrogate escapes
as invalid JSON (400), rather than allowing JSON decoding to replace characters.
An unreadable or invalid overlay returns a redacted 503; external creation, replacement or deletion conflicts with the revision. Snapshots and all live
service-name reads share one synchronization boundary; listener concurrency is
immutable until restart.
