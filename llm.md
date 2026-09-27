# Stellar Jay agent guide

Use this document as the operating contract for an agent that reads or writes
Stellar Jay. For deployment and human administration, use `README.md` and `docs/`.

## What Stellar Jay is

Stellar Jay is an authenticated, append-only event store for durable business facts.
It records who asserted something, when it was asserted, the command that caused
the assertion, and an encrypted JSON payload. Its hash-linked history is the
source of truth.

Stellar Jay does not decide whether a fact is true or materialize current entity
state. It does not enforce a domain schema unless an operator has installed a
catalog. Installed types and commands are an exact list outside the event
history. Tokens with no `allow` block keep role-wide access, except that an
enforced catalog rejects types and commands that are not installed. The
consuming agent must still validate evidence, apply its domain rules, and
derive current state by replaying relevant events.

Prefer the MCP server when your harness supports MCP: it applies every rule in
this guide for you. See [docs/mcp.md](docs/mcp.md) for setup (`stellarjay-mcp`
locally, or `https://mcp.aviansuite.com/mcp` on AvianSuite). Otherwise use the
HTTP API as described below. Never read or edit Stellar Jay's `objects/`,
`refs/`, or `keys/` files directly.

## Required connection inputs

- `STELLARJAY_URL`: HTTPS origin, without a trailing slash.
- `STELLARJAY_TOKEN`: bearer token assigned to this agent.
- Role: `reader`, `writer`, `operator`, or `admin`. `reader`, `writer`, and
  `admin` are cumulative. `operator` only manages the catalog and cannot read
  or append facts. Use `operator` for the agent that installs types, and a
  separate `writer` for the agent that appends them. Do not use `admin` for
  either job.

Never put a token in a URL, payload, log, source file, prompt transcript, or
idempotency key. Send it only in the `Authorization` header. Use the lowest role
that can complete the task.

Tokens can expire at their `not_after` boundary. Treat `401` as a credential
incident or rotation event; never paste the rejected token into a prompt, ticket,
or log while diagnosing it.

All `/v1` requests use:

```http
Authorization: Bearer <token>
```

Requests with JSON bodies also use:

```http
Content-Type: application/json
```

## Read workflow

1. Confirm availability with `GET /health/ready`. This route needs no token and
   discloses no facts.
2. For a dependent write, fetch `GET /v1/root` and retain that live root as the
   write's `expected_root`. It is not the incremental replay cursor.
3. The replay cursor is the hash of the last event this client fully applied in
   a prior session. Read with `GET /v1/events?after=<last-applied-hash>&limit=100`,
   omitting `after` for a cold replay. Capture this first response's `root` as
   the target replay boundary.
4. Payloads are omitted by default. Classify the ordered metadata by `type`, then
   retrieve only required payloads with `POST /v1/events/payloads`, passing the
   captured root and at most 100 `event_ids`. The compatibility option
   `include_payload=true` still decrypts every event in a page.
5. If the first response has no events, stop before requesting another page. The
   client is already caught up when the response root equals its cursor; an
   empty store likewise returns an empty root and no events.
6. Otherwise, apply events in order and stop immediately when an event's hash
   equals the captured target. Do not apply newer events that follow it in the
   same page.
7. Until the target is reached, take the final applied event's `hash` and request
   the next page with both `after=<hash>` and `root=<captured-root>`. The server
   holds page contents and `has_more` to that boundary despite concurrent appends.
8. After reaching the target, persist it as the new replay cursor. Never persist
   a newer root reported by a later page unless every event through it was
   applied.

Example:

```sh
curl -fsS \
  -H "Authorization: Bearer $STELLARJAY_TOKEN" \
  "$STELLARJAY_URL/v1/events?after=$LAST_APPLIED_HASH&root=$CAPTURED_ROOT&limit=100"
```

On the first request, omit `root`; that response captures the stable boundary.
Later responses echo the supplied root and calculate `has_more` relative to it.
If the first page is empty and its root equals the replay cursor, no catch-up is
needed. If the cached `after` hash returns structured
`404 not_found`, discard the checkpoint and replay from the beginning; a store
restore or replacement may have selected a different history. A missing root or
a cursor beyond it returns `409 conflict` and the scan must restart.

Do not treat the last event for an entity as current state unless the domain's
replay rules say that is correct. Corrections, retractions, approvals, and
superseding assertions may all change how earlier events are interpreted.

## Write workflow

Every append requires both optimistic concurrency and retry identity:

- `expected_root`: the exact value from the most recent `GET /v1/root`. Use `""`
  only when the database is empty.
- `Idempotency-Key`: a stable identifier for one logical operation, 8-200
  characters. Prefer an immutable upstream operation ID or a UUID stored before
  the first attempt. Never generate a new key merely because a request timed out.

Example:

```sh
curl -fsS -X POST "$STELLARJAY_URL/v1/events" \
  -H "Authorization: Bearer $STELLARJAY_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: crm-sync-contact-42-v7" \
  --data '{
    "type": "business.fact",
    "entity_id": "customer:42",
    "command": "fact assert",
    "payload": {
      "predicate": "primary_contact",
      "value": "Ada Lovelace",
      "observed_at": "2026-07-17T20:00:00Z",
      "evidence": {"kind": "crm_record", "ref": "contact:42"}
    },
    "expected_root": "sha256:..."
  }'
```

The server derives `actor`, `role`, `created_at`, parents, encryption fields, and
hash. Do not include or attempt to override them.

A token may carry an `allow` block. If it does, `type` must match
`namespace.name` or `namespace.*` inside that one namespace, and `command` must
be listed when the token lists commands. If `allow.refs` is set, named-ref
updates must match an exact name or a prefix ending in `*`. A token without
`allow` is not narrowed by this rule. When a catalog is enforced, the type and
command must also be installed, including for unscoped writers. `403
permission_denied` on a write means stop. Do not invent a new type or command
and retry.

Interpret a successful response as follows:

- `201` and `replayed: false`: a new event was committed.
- `200` and `replayed: true`: this exact logical request was already committed;
  treat it as success and use the returned hash.
- `root`: the current database root after the request. It may be newer than
  `hash` when an old successful request is replayed.

## Conflict and retry algorithm

On a timeout, connection loss, or unknown result:

1. Retry the same event with the same `Idempotency-Key`. The type, entity ID,
   command and payload must match; `expected_root` may be the newer root.
2. If the first request committed, Stellar Jay returns the original hash with
   `replayed: true`.
3. If it did not commit, the retry can commit normally if the expected root is
   still current.

On `409 conflict` with a root-changed message:

1. Fetch the current root and events after the root on which the decision was
   based.
2. Re-evaluate the operation against those intervening facts.
3. If the operation is still valid, submit it against the new root. It is safe to
   retain the idempotency key when the stale request was definitively rejected.
4. If the operation is no longer valid, do not append it. Report the conflict to
   the caller with the facts that changed the decision.

On `409 conflict` saying the request ID was used for different content, stop.
This is a client bug or an idempotency-key collision. Never resolve it by silently
inventing a new key.

Never blindly loop on `409`. Concurrency conflicts require domain reconciliation.

## Fact modeling conventions

Stellar Jay accepts arbitrary JSON, but agents should keep a stable contract:

- `type`: namespaced event category with stable semantics, such as
  `business.fact`, `crm.customer.updated`, or `policy.approved`.
- `entity_id`: stable domain identifier, not a display name.
- `command`: concise intent that produced the event, such as `fact assert`,
  `fact correct`, or `policy approve`.
- `payload`: the assertion plus the evidence needed to independently understand
  it. Prefer references to authoritative sources over unsupported prose.

When useful, include these payload fields:

- `predicate` and `value` for an asserted fact;
- `observed_at` for the source observation time;
- `evidence` with a source kind and durable reference;
- `confidence` only when uncertainty is meaningful and its scale is defined;
- `supersedes` or `retracts` containing a prior Stellar Jay hash;
- `reason` for corrections, retractions, and approvals.

Never mutate an earlier event. To correct or retract a fact, append a new event
that references the old hash and explains the change. Preserve the evidence that
led to both states.

Do not store plaintext secrets merely because Stellar Jay encrypts payloads. Store a
secret-manager reference when possible. Event metadata—type, entity ID, actor,
role, command, time, and graph shape—is not encrypted.

## Named refs

Named refs are durable checkpoints, not mutable facts.

- Read: `GET /v1/refs/{name}`.
- Write: `PUT /v1/refs/{name}` with
  `{"root":"sha256:new","expected_root":"sha256:old"}`. Use an empty
  `expected_root` only to create a ref that does not exist.

Use simple file-safe names. The root must already exist. A ref does not create a
branch or freeze the current database root. On `409`, reread the ref and
reconcile; never overwrite a checkpoint that moved concurrently.

## Error handling

Errors have this structure:

```json
{"error":{"code":"conflict","message":"root changed"}}
```

Handle codes deliberately:

- `validation_error`: fix the request; do not retry unchanged.
- `permission_denied`: stop. Do not retry with a different type, command, or
  ref. The credential is outside its role, its `allow` block, or the installed
  catalog. Installing a type is an operator action, not a writer retry.
- `not_found`: refresh the root, cursor, or named-ref assumption.
- `conflict`: follow the conflict algorithm above.
- `integrity_error`: stop writes and alert an operator; stored data failed a
  structural, hash, or decryption check.
- `capacity_exceeded`: an administrative snapshot lacks its configured free
  space reserve; free or extend backup storage before retrying.
- `internal_error`: retry the identical idempotent request with bounded backoff;
  alert an operator if it persists.

Also expect HTTP `401` for a missing or invalid token, `403` for insufficient
role, `413` for a body over 1 MiB, `415` for a non-JSON body, `503` with
`status: not_ready` when the store head cannot be verified, and `507` when a
snapshot would violate the free-space reserve.

Use bounded exponential backoff with jitter for transient transport and server
errors. Do not retry validation, authentication, authorization, or semantic
conflicts without changing the underlying condition.

## Administrative operations

Ordinary fact agents should not receive `admin` credentials.

- `POST /v1/admin/verify` walks the reachable history and authenticates every
  encrypted payload.
- `POST /v1/admin/snapshots` writes a consistent encrypted archive that excludes
  the data key.

A successful snapshot on the Stellar Jay host is not yet a backup. An operator must
copy it off-host and retain the data key in a separate failure domain.

## Non-negotiable invariants

- Use HTTPS except for an explicitly local test.
- Treat the bearer token and decrypted payloads as sensitive.
- Put PII, account numbers, tax identifiers, and human-readable financial detail
  in encrypted payloads only. Metadata (`type`, `entity_id`, actor, role,
  command, timestamp, and parents) is plaintext; use opaque identifiers there.
- Read before making a decision; fetch a root immediately before writing.
- Reuse the same idempotency key and body for an ambiguous retry.
- Reconcile rather than overwrite when the root changes.
- Append corrections; never rewrite history.
- Never operate on Stellar Jay storage files directly.
- Never run multiple Stellar Jay server replicas against the same volume.
- Never claim that tamper evidence proves a business assertion is true.
