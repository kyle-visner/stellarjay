# Hosted architecture

## Trust and data flow

```text
agent or operator
    |
    | HTTPS + bearer credential
    v
Caddy (public ports 80/443)
    |
    | private Compose network
    v
Stellar Jay server (single non-root writer)
    |                    |
    | encrypted nodes    | consistent encrypted snapshots
    v                    v
data volume          backup volume -> mandatory off-host copy

data-key file and credential hashes are mounted as separate Docker secrets
```

The service is intentionally single-writer. The current storage format is a
linear Merkle chain with a mutable root ref, not a distributed consensus
protocol. One process serializes writes with an in-process lock and holds an
advisory volume lock at `.writer.lock`; a second process fails to open the same
store. Refs are atomically replaced only after a content-addressed node is
durable. Do not run multiple Stellar Jay replicas against one volume, including on a
shared network filesystem whose locking behavior has not been proven.

Unix-family builds use kernel-released `flock` or `fcntl` advisory locks, so an
unclean process exit does not strand ownership. The portable fallback for
non-Unix targets uses exclusive file creation; after a crash on such a target,
an operator must confirm no Stellar Jay process owns the store and remove the stale
`.writer.lock` before reopening it.

## Write contract

Every hosted write carries two independent safety controls:

1. `expected_root` implements compare-and-swap. If another writer committed
   first, the server returns `409 conflict` and the caller must reread/reconcile.
2. `Idempotency-Key` identifies one logical operation for one credential. The
   server hashes the credential ID plus key into the node. Retrying identical
   content returns the original node, even after later events; using the same key
   for different content returns `409 conflict`.

The authenticated credential supplies the node's actor and role. Clients cannot
claim another identity in the request body.

## Storage and durability

Each node contains metadata, parent hashes, and an AES-256-GCM sealed JSON
payload. The SHA-256 node address covers both metadata and ciphertext. New node
files are created without overwriting an existing address, synced, and then the
root ref is written through a same-directory temporary file, rename, and
directory sync.

This ordering means a crash can leave an unreachable node, but cannot make the
root reference a partially written node. Snapshots include all encrypted nodes
and refs, so an unreachable node is retained for forensic recovery.

At open, Stellar Jay verifies reachable history and builds lightweight in-memory
indexes for event position and request identity. That startup work is linear in
history size. It makes idempotent replay lookup constant-time and lets the read
API load only the requested page instead of materializing the full history.

Replay separates metadata discovery from payload access. A metadata page reads
ordered identity, parent, type, actor/role, command, and timestamp fields from
the verified in-memory index without reopening, decrypting, or authenticating
sealed payloads. Clients bind subsequent pages
to the first observed root, classify domain events, then retrieve selected
payloads by opaque event identity in bounded batches. Selected reads verify the
full content address and AES-GCM authentication before returning plaintext. This
means a damaged or key-mismatched foreign payload does not block another
application's metadata scan or valid selected payloads. Full administrative
verification still authenticates every reachable node and payload.

## Scaling boundary

The reference deployment scales clients, not writers. It is appropriate for a
small organization whose fact stream fits on one durable host and whose recovery
objective is satisfied by frequent off-host snapshots.

A future multi-region version must replace the filesystem ref update with a
transactional coordination layer (for example, a database row locked by version)
or define explicit branch/merge semantics. Placing this version on a shared
network filesystem or increasing the replica count is not a valid substitute.
