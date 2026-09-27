<p align="center">
  <img src="docs/assets/jaybase-logo.png" alt="Jaybase logo" width="900">
</p>

# Jaybase

## TL;DR

Jaybase is an append-only fact store for AI agents trusted with critical
business data. Agents can add flexible JSON facts, but the hosted API cannot
rewrite or delete history. Every write is attributed, encrypted, safe to retry,
checked for stale state, and available for replay.

Requires Go 1.22 or later:

```sh
git clone https://github.com/kyle-visner/jaybase.git
cd jaybase
go install ./cmd/jaybase-server
jaybase-server init ./secrets
```

The initializer prints reader, writer, and admin tokens once. Save them in a
password manager, then continue to [Run Jaybase](#run-jaybase).

## Who Jaybase is for

Jaybase is for developers and small teams moving from read-only copilots to
agents that are allowed to operate. It fits accounting, operations, compliance,
approvals, and other work where agents write critical data, mistakes must stay
visible and correctable, and fact shapes evolve with the job.

Jaybase is designed for single-tenant systems: one organization, one trust
boundary, and one writer process per store. Many agents and applications can
share that store, including dashboards, internal tools, APIs, and automated
workflows. Jaybase is not meant to be the globally distributed, multi-tenant
backend for a web application.

## Why Jaybase exists

Traditional databases assume deterministic application code owns every read and
write. Agents make judgment calls, retry uncertain work, and sometimes behave in
unexpected ways—at machine speed. A mutable database can turn one bad decision,
runaway loop, or malicious instruction into lost source data before anyone
notices.

| Agent risk | Jaybase response |
| --- | --- |
| Destructive behavior or a wrong decision | Append-only writes, credential roles, throttling, and corrections that preserve evidence |
| A timeout or stale decision | Return the original retry result or reject a write based on old history |
| A changing job | Accept new JSON fields and fact types without rewriting old facts |

You can build these protections around a general-purpose database. Jaybase makes
them part of every write instead of leaving them to each application.

Jaybase does not decide whether a fact is true. An authorized agent can still
write a bad fact; Jaybase keeps that action visible and correctable.

## How it works

Jaybase stores a linear chain of events. Each event records what happened, who
did it, an encrypted JSON payload, and the event before it. Payloads stay
flexible; the history rules do not.

The normal write flow is:

1. Read the current `root`.
2. Submit a fact with that `expected_root` and a stable `Idempotency-Key`.
3. Jaybase derives the actor, encrypts and hashes the event, writes it, and
   advances the root.
4. Identical retries return the original event. Stale roots and reused keys with
   different content return `409 conflict`.

Each event address depends on the event before it. Changing old content changes
the hashes that follow, so an off-host copy of the root can detect rewritten or
replaced history.

Corrections, retractions, and approvals are new events, never edits. The hosted
API has no update or delete path for history, and callers cannot choose their own
identity. One writer process serializes writes for each data volume; many agents
can use that process, but Jaybase is not a distributed consensus system.

## Run Jaybase

### Deploy the hosted service

Prerequisites: a Linux host with Docker Compose, ports 80 and 443 reachable, and
an A/AAAA record pointing a domain at the host.

```sh
cp .env.example .env
# Edit .env and set JAYBASE_DOMAIN.

jaybase-server init ./secrets

docker compose up -d --build
docker compose ps
curl https://jaybase.example.com/health/ready
curl https://jaybase.example.com/llm.txt
```

The origin is the website. `/` links to `/llm.txt`, which is the agent setup contract (CLI + `JAYBASE_URL` / `JAYBASE_TOKEN`, not per-app MCP).

The initializer will not replace existing secrets. The server requires an
external data key and hashed credential file.

### Append a fact

Fetch the current root first:

```sh
export JAYBASE_URL=https://jaybase.example.com
export JAYBASE_TOKEN='the-writer-token'

curl -fsS \
  -H "Authorization: Bearer $JAYBASE_TOKEN" \
  "$JAYBASE_URL/v1/root"
```

Use the returned root as `expected_root` and choose one stable idempotency key
for the logical operation. Use an empty string only for the first event in a new
database.

```sh
curl -fsS -X POST "$JAYBASE_URL/v1/events" \
  -H "Authorization: Bearer $JAYBASE_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: fact-primary-contact-v1-7f3d2a" \
  --data '{
    "type": "business.fact",
    "entity_id": "01JOPAQUE8F3K2M7Q9R4T6V1WX",
    "command": "fact assert",
    "payload": {"primary_contact": "Ada Lovelace"},
    "expected_root": "sha256:root-from-the-previous-response"
  }'
```

See the [API guide](docs/api.md) for metadata-first replay, selective bounded
payload reads, stable-root pagination, refs, snapshots, and administrative
endpoints.

### Use the embedded Go library

```go
store, err := jaybase.OpenStore(".jaybase")
if err != nil { /* handle error */ }
defer store.Close()
root, err := store.Append(jaybase.Context{Actor: "agent"}, jaybase.AppendOptions{
    Type: "business.fact", Command: "fact assert", Payload: fact,
})
```

`OpenStore` is a local-development convenience that co-locates the key and data.
Production processes must use `OpenStoreWithDataKey`; the server enforces this.

## Production boundaries

- One process owns each writable data volume.
- Caddy handles HTTPS; bearer credentials provide `reader`, `writer`, or `admin`
  access. An optional `operator` role manages the catalog and cannot append.
- Omitted token scopes and an unconfigured catalog preserve open writes. A
  scoped token, or an enforced catalog, rejects appends outside that boundary.
- Payloads are encrypted at rest, with the data key stored outside the volume
  and snapshots.
- Snapshots should be copied off-host.
- Containers run as non-root with a read-only root filesystem.

Read the [architecture](docs/architecture.md), [security](docs/security.md),
[API](docs/api.md), and [operations](docs/operations.md) guides before running
Jaybase with sensitive data. Point an agent at `$JAYBASE_URL/llm.txt` to set
up. [llm.md](llm.md) is the full write/replay contract.

## Verify

```sh
GOCACHE=/tmp/jaybase-gocache go test -race ./...
GOCACHE=/tmp/jaybase-gocache go vet ./...
docker compose config
docker build -t jaybase:test .
```

## License

AGPL-3.0-or-later. See `LICENSE`.
