# Operations runbook

## Initial deployment

1. Provision a patched Linux host with persistent storage and Docker Compose.
2. Point the chosen DNS name at it and allow inbound TCP 80/443 and UDP 443.
3. Clone the repository, copy `.env.example` to `.env`, and set the domain.
4. Run `go run ./cmd/stellarjay-server init ./secrets` on a trusted machine. Save
   the printed tokens in a password manager.
5. Back up `secrets/data_key` to a separate secret store. Losing it makes every
   payload unrecoverable.
6. Run `docker compose up -d --build` and check `docker compose ps`.
7. Call `/health/ready`, append a test fact, read it back, trigger a snapshot, and
   copy that snapshot off-host.

Compose publishes only Caddy. Stellar Jay port 8080 stays on the private Compose
network.

The reference Compose file enforces per-credential API and global failed
authentication limits. Defaults are 600 and 30 per minute; tune
`STELLARJAY_RATE_LIMIT_PER_MINUTE` and `STELLARJAY_FAILED_AUTH_LIMIT_PER_MINUTE` after
measuring normal agents. A limited call returns `429` with `Retry-After: 60`.
Use Caddy logs and the provider firewall/WAF for network allowlists, volumetric
controls, and alerts on bursts of `401` or any `429`.

## Routine backup

Call the snapshot endpoint with an admin token:

```sh
curl -fsS -X POST \
  -H "Authorization: Bearer $STELLARJAY_ADMIN_TOKEN" \
  "$STELLARJAY_URL/v1/admin/snapshots"
```

The response names an archive in `/var/backups/stellarjay` inside the Stellar Jay
container. Copy it to a different machine, account, or object-storage service.
A snapshot left only in the local Docker volume is not a backup.

`STELLARJAY_SNAPSHOT_RETENTION` (default `24`) bounds the managed local archives.
After each successful snapshot, Stellar Jay deletes the oldest matching
`stellarjay-*.tar.gz` files above that count. `STELLARJAY_SNAPSHOT_MIN_FREE_BYTES`
(default `536870912`, or 512 MiB) is preserved in addition to the estimated
snapshot size; the endpoint returns `507` before writing when space is
insufficient. Set the value explicitly to `0` to disable the reserve on a
dedicated backup volume. These controls protect the host but do not replace
off-host retention. If retention cleanup fails after an archive is durable, the
endpoint still returns that archive's `201` result and emits an error log for
operators; automation must monitor those logs and local volume use.

Keep these items in separate failure domains:

- snapshot archive;
- `secrets/data_key`;
- an operator record containing the snapshot root and timestamp.

After export, prove that the recorded root is still in live history:

```sh
curl -fsS -G \
  -H "Authorization: Bearer $STELLARJAY_ADMIN_TOKEN" \
  --data-urlencode "root=$OFF_HOST_ROOT" \
  "$STELLARJAY_URL/v1/admin/check-root"
```

Set `STELLARJAY_MINIMUM_ROOT` to the last off-host pin and recreate Stellar Jay when
readiness should enforce the same condition. Advance it only after exporting and
verifying the corresponding snapshot. When the pin is absent, Stellar Jay rejects
event appends and named-ref updates with `503 integrity_error` even if traffic can
still reach the process; reads and admin verification remain available for
forensic investigation.

Use a scheduler on the host or in an external automation system to trigger and
export snapshots. Do not place the admin token directly in a crontab; read it
from a root-owned credential file or secret manager.

Compose also sets default limits of one CPU and 512 MiB for Stellar Jay and half a
CPU and 256 MiB for Caddy. Override `STELLARJAY_CPUS`, `STELLARJAY_MEMORY_LIMIT`,
`CADDY_CPUS`, and `CADDY_MEMORY_LIMIT` in `.env` only after measuring workload
and restore behavior.

## Integrity check

```sh
curl -fsS -X POST \
  -H "Authorization: Bearer $STELLARJAY_ADMIN_TOKEN" \
  "$STELLARJAY_URL/v1/admin/verify"
```

Run this periodically and before declaring a backup good. It checks the hash
chain and authenticates every encrypted payload. Verification processes one
node at a time instead of materializing another history copy, but remains an
O(history) admin operation. Schedule it and monitor its logged duration.

## Credential rotation

Add an expiring replacement, recreate Stellar Jay, move the client, revoke the old
credential, and recreate again:

```sh
go run ./cmd/stellarjay-server add-token \
  ./secrets/auth.json writer-next writer "$NOT_AFTER_RFC3339"
docker compose up -d --force-recreate stellarjay
go run ./cmd/stellarjay-server revoke-token ./secrets/auth.json writer-old
docker compose up -d --force-recreate stellarjay
```

Set `NOT_AFTER_RFC3339` to a reviewed near-term UTC boundary. `add-token` prints
plaintext once; send it only to a password manager or trusted secret-import
command. Expiry is enforced without reload, while file changes
need recreation. If a token enters a ticket, log, chat, prompt, or shell history,
use this procedure immediately and audit that principal's earlier requests.

Scope a writer without changing existing unscoped tokens:

```sh
go run ./cmd/stellarjay-server add-token \
  ./secrets/auth.json magpie-writer writer \
  --allow-type 'magpie.*' --allow-command journal.post --allow-ref 'magpie-*'
```

`NOT_AFTER` still comes before any `--allow-*` flag. Omit `allow` and the new
token has the same role-wide access as before. Recreate Stellar Jay to load it.

## Catalog

The catalog is optional and is not part of the event history. Leave
`STELLARJAY_CATALOG_FILE` unset to keep open writes. Point it at a writable file
on the data volume, not at the read-only auth secret:

```sh
# in the environment of the running service
STELLARJAY_CATALOG_FILE=/var/lib/stellarjay/catalog.json
```

An operator token can install a type over HTTP. That turns enforcement on
immediately and does not append an event:

```sh
curl -fsS -X POST "$STELLARJAY_URL/v1/admin/catalog/entries" \
  -H "Authorization: Bearer $STELLARJAY_OPERATOR_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"type":"magpie.journal","commands":["journal.post"]}'
```

The same file can be edited from the host. A running process keeps the catalog
it loaded until recreate, unless the change came through the HTTP API:

```sh
go run ./cmd/stellarjay-server catalog install \
  /var/lib/stellarjay/catalog.json magpie.journal journal.post
go run ./cmd/stellarjay-server catalog show /var/lib/stellarjay/catalog.json
go run ./cmd/stellarjay-server catalog enforce /var/lib/stellarjay/catalog.json false
```

`enforce false` is the host-only way back to open writes. Agents cannot call
it. After the first install, appends of types or commands that are not listed
fail with `403` for writers and for admin.

## Data-key migration after compromise

Migration is offline and writes a new store; it never edits the source. New
ciphertext means every node hash changes, including roots held by named refs.

1. Isolate the incident, export logs, stop Stellar Jay, and snapshot the source for
   forensics. Ensure no process can append.
2. Generate a new 32-byte key in a trusted secret manager. Materialize old and
   new keys temporarily as mode-0600 files outside both data directories.
3. Migrate into a previously nonexistent destination:

   ```sh
   umask 077
   go run ./cmd/stellarjay-server migrate-key \
     /srv/stellarjay-old /srv/stellarjay-new /run/keys/old /run/keys/new \
     > /secure/stellarjay-key-migration.json
   ```

4. Preserve that mode-0600 JSON result off-host. It records source and destination
   roots, counts, and the complete `hash_map`. Start the destination with the new
   external key, run admin verify, compare counts and representative facts, and
   export a new snapshot.
5. Named refs are translated automatically. Translate external replay checkpoints
   through `hash_map` or force a cold replay. Payload bytes are deliberately not
   rewritten, so domain fields such as `supersedes` still contain source hashes;
   consumers must retain the map when resolving them or append explicit mapping
   facts after cutover. Resolve all ambiguous pre-cutover writes and never retry
   an old idempotency key against the migrated store.
6. Set the minimum-root pin to the destination root, then cut over traffic.
   Retain the source read-only under incident policy and remove temporary key
   files using the secret-manager procedure.

If migration fails before all nodes and named refs are durable, Stellar Jay closes
and removes the destination it created; a cleanup failure is joined into the
reported error with the exact remaining path. If only the final store close
fails, Stellar Jay preserves the completed destination and emits its JSON migration
manifest before the CLI exits nonzero. Inspect and verify that destination rather
than deleting it automatically. Retry into a nonexistent directory only after
confirming that the earlier path is absent or intentionally retained.
Re-encryption limits future exposure; it cannot undo access to the compromised
old key. A KMS/HSM should unwrap into the existing read-only key-file mount,
never onto the data volume.

## Restore drill

Always restore into a new empty volume first:

1. Stop the candidate Stellar Jay instance.
2. Extract the archive into a new empty data volume. The archive contains
   `objects/` and `refs/`; it intentionally contains no `keys/` directory.
3. Mount the original data-key secret and the desired auth file.
4. Start Stellar Jay without exposing it publicly and wait for readiness.
5. Call `/v1/admin/verify`, compare the returned root with the off-host backup
   record, and read representative facts.
6. Only then switch traffic or DNS to the restored instance.

Never test restoration by overwriting the only production volume.

## Migrating an existing local store

An existing `.stellarjay` directory can move without rewriting its history:

1. Stop every process that can append to the local store.
2. Save `.stellarjay/keys/data.key` as the hosted `secrets/data_key`.
3. Copy `.stellarjay/objects` and `.stellarjay/refs` into a new hosted data volume.
4. Do not copy `.stellarjay/keys` into that volume.
5. Start hosted Stellar Jay with the saved external key and run the admin verify call.
6. Compare the hosted root with the last local root before allowing writes.

Keep the original local directory read-only until the hosted restore and backup
drill succeeds.

## Upgrading from Jaybase

Stellar Jay was called Jaybase before v0.4.0. Stored history is unchanged, so an
upgrade needs no migration:

- `JAYBASE_*` environment variables are still read when the matching
  `STELLARJAY_*` variable is unset. The server logs a warning for each one;
  rename them when convenient.
- `compose.yaml` keeps the old Compose volume names, so the existing store and
  Caddy certificates are reused.
- The image still ships `/jaybase-server` beside `/stellarjay-server`.
- A local store in `.jaybase` is used when `.stellarjay` does not exist.
- Snapshot retention counts both `jaybase-*.tar.gz` and `stellarjay-*.tar.gz`.

## Updating

Before an update, create and export a snapshot. Then rebuild and recreate:

```sh
git pull --ff-only
docker compose build --pull stellarjay
docker compose pull caddy
docker compose up -d
docker compose ps
```

Finish with readiness, integrity, append, and replay checks. Review release
changes before updating across schema versions.

## Incident priorities

If a token is exposed, use the add/recreate/revoke/recreate procedure immediately.
If the data key may be exposed, isolate the host, preserve logs and volumes, and
treat every payload as compromised. Use the offline migration above; there is no
in-place or zero-downtime key rotation.

If integrity verification fails, stop writes, preserve the current volume, and
compare its root and node set with off-host snapshots. Do not repair files in
place before capturing forensic copies.
