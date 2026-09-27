# HTTP API

All responses are JSON. Routes under `/v1` require
`Authorization: Bearer <token>`. Error responses have this shape:

```json
{"error":{"code":"conflict","message":"root changed"}}
```

Roles are cumulative for `reader`, `writer`, and `admin`: writers can read;
admins can read, write, and operate. `operator` is not on that ladder. It can
manage the catalog and cannot read or append facts. Existing tokens with no
`allow` block keep role-wide access.

An unconfigured catalog, or a configured catalog that has never been enforced,
still accepts any event type. Installing a type turns enforcement on. Appends
must then use an installed type and command. That check does not rewrite
history. Removing the last type leaves enforcement on, so later appends fail
closed instead of returning the store to open writes. Host tooling, not the
HTTP API, can turn enforcement back off.

## Health

`GET /health/live` reports that the process is running. `GET /health/ready`
verifies that the current root addresses a valid, complete head node in constant
time. Neither route exposes data or requires authentication; the admin verify
route performs the full history and decryption check. A failed readiness check
returns `503` with `status: "not_ready"` and an `integrity_error`.
When `STELLARJAY_MINIMUM_ROOT` is set, readiness also requires that independently
pinned root to remain in live history. Event appends and named-ref updates enforce
the same pin inside the authenticated handler and return `503 integrity_error`
when it is absent; protection does not depend only on external health routing.

## Root

`GET /v1/root` requires `reader` and returns:

```json
{"root":"sha256:..."}
```

The root is an empty string for a new database.

## Append event

`POST /v1/events` requires `writer`, `Content-Type: application/json`, and an
`Idempotency-Key` of 8-200 characters.

```json
{
  "type": "business.fact",
  "entity_id": "customer:42",
  "command": "fact assert",
  "payload": {"status": "active"},
  "expected_root": "sha256:..."
}
```

`expected_root` is required. Use `""` only for the first event. A successful new
append returns `201`; an identical retry returns `200` with `replayed: true`.
A stale root or reused idempotency key with different content returns `409`.
The credential's actor and role are still taken from the token. Clients cannot
choose them.

When the token has an `allow` block, the type must match one pattern
(`namespace.name` or `namespace.*`, one namespace only) and, if `commands` is
present, the command must be listed exactly. A token with no `allow` block is
unchanged. Named-ref updates are likewise unchanged unless `allow.refs` is set;
a ref pattern is an exact file-safe name or a prefix ending in `*`, such as
`magpie-*`. A mismatch returns `403 permission_denied` and does not append.
Stop; do not retry the same operation under a different type.

## Catalog

Catalog routes require `operator` (or `admin`) and a configured
`STELLARJAY_CATALOG_FILE`. Without that file they return `503`. They edit the
catalog file only. They do not append events.

- `GET /v1/admin/catalog` returns `{"enforced":false,"entries":[]}`.
- `POST /v1/admin/catalog/entries` with
  `{"type":"magpie.journal","commands":["journal.post"]}` installs one exact
  type and turns enforcement on. A new type returns `201`. Repeating that
  install returns `200`.
- `DELETE /v1/admin/catalog/entries/{type}` removes that type and does not turn
  enforcement off.

Writers cannot call these routes. An enforced catalog rejects every writer,
including an unscoped writer or `admin`, when the type or command is not
installed. `operator` cannot append, so the credential that edits the schema
cannot also write facts.

`stellarjay-server catalog enforce CATALOG_FILE false` is the host escape hatch
back to open writes. The HTTP API does not expose it.

## Replay events

`GET /v1/events` requires `reader`.

Query parameters:

- `limit`: 1-1000, default 100.
- `after`: return nodes strictly after this hash in the current history.
- `root`: optional observed root that bounds the page. Capture it from the first
  page and send it on every later page of the same scan.
- `include_payload=true`: decrypt and include payloads. Payloads are omitted by
  default. Payload-inclusive compatibility pages are limited to 100 events even
  when `limit` is larger. If the encoded response exceeds the configured
  application response limit, Stellar Jay rejects it with `507 capacity_exceeded`.

Each event includes `event_id` and `hash` (the same opaque content identity in
this version), type, entity ID, parent hashes, actor/role, command, timestamp,
and request ID. Metadata-only pages do not decrypt or authenticate sealed
payloads. They therefore remain usable when an unrelated payload is corrupt;
payload authentication is deferred until that event is selected.

The response includes `events`, `root`, and `has_more`. Without a `root` query,
`root` is the live history tip captured atomically with the page. With a `root`
query, both the returned events and `has_more` are bounded to that exact observed
root, even when concurrent appends advance the live history. An absent observed
root or an `after` cursor beyond it returns `409 conflict`; an unknown `after`
cursor returns `404 not_found`.

For a concurrency-safe incremental replay:

1. Request the first page after the client's checkpoint—the hash of the last
   event it fully applied—and capture that response's `root` as the target
   replay boundary. Omit `after` for a cold replay.
2. If the first response has no events, stop before entering the pagination
   loop. The client is already caught up when `root` equals its checkpoint; an
   empty store likewise returns an empty target and no events.
3. Otherwise, classify the metadata and request later pages with both the final
   event ID as `after` and the captured target as `root`.
4. Fetch only needed payloads through the bounded endpoint below, apply events
   in chain order, and persist the target after all selected facts through it
   have been applied.

Stellar Jay's history is a linear append-only chain, so a captured target remains
reachable when a concurrent writer advances the live root. A cached `after`
hash that is not in the current history returns structured `404 not_found`; the
client must invalidate that checkpoint and perform a cold replay. This can
happen after restoring or replacing a store. Payload omission and
`include_payload=true` behave identically during bounded replay.

## Selective payload retrieval

`POST /v1/events/payloads` requires `reader` and `Content-Type:
application/json`:

```json
{
  "root": "sha256:observed-replay-root",
  "event_ids": ["sha256:first-event", "sha256:second-event"]
}
```

The request must contain 1-100 unique event identities. Every identity must
belong to this store and occur at or before `root`. Results retain request order
and include integrity-checked plaintext:

```json
{
  "root": "sha256:observed-replay-root",
  "payloads": [
    {"event_id":"sha256:first-event","hash":"sha256:first-event","payload":{"status":"active"}}
  ]
}
```

The JSON response may not exceed the server's configured application body
limit (1 MiB by default); an oversized result returns `507 capacity_exceeded`.
The normal authenticated per-principal rate limit and HTTP read/write timeouts
also apply. Audit logs record the principal, observed root, selected count, and
event identities, but never plaintext. A corrupt selected event returns
`500 integrity_error`; corrupt events not selected do not affect the batch.

## Named refs

- `GET /v1/refs/{name}` requires `reader`.
- `PUT /v1/refs/{name}` requires `writer` and body
  `{"root":"sha256:new","expected_root":"sha256:old"}`. Use an empty
  `expected_root` only when creating a new ref.

A ref name must be a simple filename. The selected root must name an existing,
valid node. If the current ref differs from `expected_root`, the update returns
`409 conflict` without overwriting it.

## Administration

- `POST /v1/admin/verify` requires `admin`. It verifies every reachable node and
  decrypts every reachable payload.
- `POST /v1/admin/snapshots` requires `admin`. It creates a consistent archive in
  the configured backup volume and returns its filename, root, timestamp, and
  node count. The archive does not include the data key. The endpoint refuses
  the request with `507 capacity_exceeded` if the estimated archive would consume
  the configured free-space reserve and prunes the oldest managed snapshots
  after a successful write. If post-write retention cleanup fails, the durable
  archive still returns `201`; the server logs the cleanup failure for operators.
- `GET /v1/admin/check-root?root=sha256:...` requires `admin`. It returns `200`
  when the supplied off-host pin is the live root or an ancestor and
  `409 integrity_error` when it is absent.

Authenticated calls are limited per principal; failed authentication is limited
globally. Configure `STELLARJAY_RATE_LIMIT_PER_MINUTE` and
`STELLARJAY_FAILED_AUTH_LIMIT_PER_MINUTE`. A limited call returns `429`, error code
`rate_limited`, and `Retry-After: 60`.

Request bodies are limited to 1 MiB at the application and 2 MiB at the proxy;
an application-limit violation returns one structured `413 validation_error`.
Stored-data validation, hash, and decryption failures return
`500 integrity_error`, not a client validation error.
