# Security model

## What the reference deployment protects

- HTTPS protects credentials and payloads in transit.
- Role-scoped bearer credentials separate read, write, and administrative work.
  `reader`, `writer`, and `admin` stay cumulative. Optional `allow` patterns
  narrow a token without changing tokens that omit them. Optional `operator`
  credentials manage the catalog and cannot append. Only SHA-256 digests are
  stored in `auth.json`; optional RFC3339 `not_after` values make credentials
  fail closed at their expiry boundary.
- AES-256-GCM protects payload confidentiality and authenticity on data and
  backup volumes. The external data key is excluded from hosted storage and
  snapshots.
- Content hashes and parent links expose modification of nodes reachable from
  the current root. An off-host root pin can be checked against live history.
- Payloads are omitted from normal replay unless the reader explicitly requests
  them. Safe read intent and administrative outcomes appear in structured logs.
- Body limits, timeouts, per-principal throttling, and global failed-auth
  throttling bound simple abuse.
- The application container is non-root and read-only except for its data and
  backup volumes and a small tmpfs; Linux capabilities are removed.

## What the operator controls

- Host access. The data key is mounted on the host and decrypted facts live in
  process memory, so restrict root and SSH access to the host.
- Who can read and write. Issue `reader` tokens to agents that need decrypted
  facts, and `writer` tokens to agents trusted to append. Every append is
  attributed to its token and can be corrected or undone.
- Root pins and snapshots. Keep them in a separate trust domain so a volume
  restored to an older root is caught; see [Rollback detection](#rollback-detection).
- Where sensitive values go. Payloads are encrypted; metadata (`type`,
  `entity_id`, `actor`, `role`, `command`, `created_at`, parents, and graph
  shape) is stored in plaintext so history can be indexed and replayed. Put
  sensitive values in payloads and use opaque identifiers in metadata.
- Key rotation. Rotate the data key with the offline migration in the
  [operations runbook](operations.md); it writes a new store with new node
  hashes.

## Financial deployment profile

Each deployment serves one organization, with one process, one volume, and one
active data key. `reader`, `writer`, and `admin` are cumulative roles, and
`allow` patterns narrow a token to a namespace. `operator` is a separate catalog
role, not a second writer. Give each party that must not share an admin its own
deployment and key.

Give an agent `operator` when it may install types, and a scoped `writer` when
it may append. Do not give that schema agent `admin`: admin remains a superset
and can still append installed types. An unenforced catalog changes nothing.
Once a type is installed, unknown types and commands are rejected for every
appender, and deleting the last type stays closed. Reopening arbitrary writes
is a host command (`catalog enforce FILE false`), not an agent request.

For financial data:

- put PII, account numbers, tax identifiers, memo text, and source documents only
  in encrypted payloads;
- use opaque random identifiers in metadata;
- give ordinary agents expiring `reader` or `writer` tokens, never `admin`;
  add `allow` when a writer must stay inside one namespace;
- deliver the data key from a secret manager or KMS/HSM-backed workflow through
  the existing read-only key-file mount;
- keep the key, snapshots, and root pins in separate failure domains; and
- use host patching, full-disk encryption, restricted SSH, and human review.

## Credential lifecycle

Create a replacement token without manually handling its digest:

```sh
go run ./cmd/stellarjay-server add-token \
  ./secrets/auth.json writer-next writer "$NOT_AFTER_RFC3339"
```

The command prints plaintext once. Store it in a password manager, recreate the
service, move clients, then revoke the old ID and recreate again:

```sh
docker compose up -d --force-recreate stellarjay
go run ./cmd/stellarjay-server revoke-token ./secrets/auth.json writer-old
docker compose up -d --force-recreate stellarjay
```

Set `NOT_AFTER_RFC3339` to a reviewed near-term UTC boundary. The argument is
optional and uses RFC3339. Expiry is enforced by a running
process, but auth-file additions and revocations require recreation.
`revoke-token` refuses to remove the final credential.

If a token appears in logs, tickets, shell history, pasted chat, or LLM context,
treat it as compromised: add a replacement, recreate, update clients, revoke,
recreate, and inspect access logs from before revocation. Never place plaintext
tokens in `auth.json`, Compose environment, command arguments, source control,
issue text, prompts, or URLs. `hash-token` remains available for manual recovery.

## Rollback detection

Retain every exported snapshot's root and timestamp off-host. Check the last pin:

```sh
curl -fsS -G \
  -H "Authorization: Bearer $STELLARJAY_ADMIN_TOKEN" \
  --data-urlencode "root=$PINNED_ROOT" \
  "$STELLARJAY_URL/v1/admin/check-root"
```

The endpoint returns `200` only when the pin is the live root or an ancestor and
`409 integrity_error` when absent. Set `STELLARJAY_MINIMUM_ROOT` to the last
independently retained pin and recreate Stellar Jay to make an absent pin fail
readiness with `503`. Event appends and named-ref updates also fail closed with
`503` while the pin is absent, even if a client bypasses health-aware routing.
Reads and verification remain available for investigation. Alert before allowing
writes. Never keep the only pin on the Stellar Jay volume.

## Host hardening checklist

- Patch hosts and images; Dependabot tracks repository dependencies.
- Permit public 80/443 and restricted administrative SSH only. Never publish
  Stellar Jay port 8080.
- Use full-disk encryption and an edge firewall or WAF. Application throttling
  complements the edge firewall, which handles volumetric traffic.
- Send JSON logs to a restricted destination. Stellar Jay never logs bodies or
  Authorization headers. Event reads record `include_payload`, the actual
  `payloads_decrypted` count, `limit`, and `after_present`. Selective payload
  reads record the principal, outcome, observed root, bounded selected count,
  and event identities without plaintext. Verify, snapshot, and root checks
  record outcome, root, node count, and duration.
- Alert on bursts of `401`, any `429`, failed admin outcomes, and unexpected
  payload-reading principals.
- Export every snapshot off-host, retain multiple generations, and compare pins.
- Run full verify and restore drills on a measured schedule.
