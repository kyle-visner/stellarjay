# How Stellar Jay works

This page holds the technical detail behind the [README](../README.md). For the
full write and replay contract agents follow, read [llm.md](../llm.md).

## Why Stellar Jay exists

Traditional databases assume deterministic application code owns every read and
write. Agents make judgment calls, retry uncertain work, and sometimes behave in
unexpected ways, at machine speed. A mutable database can turn one bad decision,
runaway loop, or malicious instruction into lost source data before anyone
notices.

| Agent risk | Stellar Jay response |
| --- | --- |
| Destructive behavior or a wrong decision | Append-only writes, credential roles, throttling, and corrections that preserve evidence |
| A timeout or stale decision | Return the original retry result or reject a write based on old history |
| A changing job | Accept new JSON fields and fact types without rewriting old facts |

You can build these protections around a general-purpose database. Stellar Jay
makes them part of every write instead of leaving them to each application.

Stellar Jay does not decide whether a fact is true. An authorized agent can still
write a bad fact; Stellar Jay keeps that action visible and correctable.

## Storage model

Stellar Jay stores a linear chain of events. Each event records what happened,
who did it, an encrypted JSON payload, and the event before it. Payloads stay
flexible; the history rules do not.

The normal write flow is:

1. Read the current `root`.
2. Submit a fact with that `expected_root` and a stable `Idempotency-Key`.
3. Stellar Jay derives the actor, encrypts and hashes the event, writes it, and
   advances the root.
4. Identical retries return the original event. Stale roots and reused keys with
   different content return `409 conflict`.

Each event address depends on the event before it. Changing old content changes
the hashes that follow, so an off-host copy of the root can detect rewritten or
replaced history.

Corrections, retractions, and approvals are new events, never edits. The hosted
API has no update or delete path for history, and callers cannot choose their own
identity. One writer process serializes writes for each data volume; many agents
can use that process, but Stellar Jay is not a distributed consensus system.

## Deployment scope

Stellar Jay is designed for single-tenant systems: one organization, one trust
boundary, and one writer process per store. Many agents and applications can
share that store, including dashboards, internal tools, APIs, and automated
workflows. Stellar Jay is not meant to be the globally distributed, multi-tenant
backend for a web application.

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

Read the [architecture](architecture.md), [security](security.md),
[API](api.md), and [operations](operations.md) guides before running Stellar Jay
with sensitive data.

## Embedded Go library

```go
store, err := stellarjay.OpenStore(".stellarjay")
if err != nil { /* handle error */ }
defer store.Close()
root, err := store.Append(stellarjay.Context{Actor: "agent"}, stellarjay.AppendOptions{
    Type: "business.fact", Command: "fact assert", Payload: fact,
})
```

`OpenStore` is a local-development convenience that co-locates the key and data.
Production processes must use `OpenStoreWithDataKey`; the server enforces this.
