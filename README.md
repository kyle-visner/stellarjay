<p align="center">
  <img src="docs/assets/stellarjay-logo.png" alt="Stellar Jay logo" width="900">
</p>

# Stellar Jay

**Stellar Jay is the open-source data store that powers
[AvianSuite](https://aviansuite.com).** AvianSuite is an agent-native business
data platform: AI agents run business processes like sales follow-up and
bookkeeping on your company's data, and every change they make is versioned,
attributed to the agent that made it, and reversible. Think of it as Git for
your business data.

Agents connect over MCP. The server is listed in the MCP registry as
`com.aviansuite/stellar-jay`, and the CRM ([Martin](https://github.com/kyle-visner/martin))
and bookkeeping ([Magpie](https://github.com/kyle-visner/magpie)) apps run on
the same store. This project was formerly called JayBase.

- **Hosted:** [AvianSuite](https://aviansuite.com) runs Stellar Jay for you.
  $20/month per store, with a 14-day free trial.
- **Self-hosted:** free and open source under the AGPL. The quick start below
  gets a store running on your machine in a few minutes.

## Agent setup

Connect an agent with the MCP server. On AvianSuite, add the remote server:

```sh
# Claude Code
claude mcp add --transport http aviansuite https://mcp.aviansuite.com/mcp
```

```json
// Cursor: .cursor/mcp.json
{"mcpServers": {"aviansuite": {"url": "https://mcp.aviansuite.com/mcp"}}}
```

In Claude and ChatGPT, add a custom connector with the same URL.

For a self-hosted store, run the local server instead:

```json
{
  "mcpServers": {
    "aviansuite": {
      "command": "stellarjay-mcp",
      "env": {"STELLARJAY_URL": "https://store.example.com", "STELLARJAY_TOKEN": "writer-token"}
    }
  }
}
```

Install it with `go install github.com/kyle-visner/stellarjay/cmd/stellarjay-mcp@latest`.
Tools and details: [docs/mcp.md](docs/mcp.md).

## Why

Clients want agents that do the work, not just read about it. But when an agent
writes straight into a CRM or ticketing system, one bad decision or runaway
loop can overwrite or delete records, and there is often no way back.

Stellar Jay is built so that can't happen:

- **Nothing is overwritten or deleted.** A correction or retraction is a new
  entry, and the original stays in history.
- **Every change has a name on it.** Each agent gets its own token, so you can
  see exactly which agent changed what, and when.
- **Any change can be undone.** Roll back everything one agent did in a time
  window, with a preview first.
- **History is tamper-evident.** If anyone rewrites or removes past entries, it
  shows.
- **Retries are safe.** An agent that retries after a timeout doesn't create
  duplicates, and a write based on stale information is refused.
- **Your data stays flexible.** Facts are JSON, so new fields and new kinds of
  records need no migrations.

Every fact stays visible and correctable: when an agent gets something wrong,
you can see exactly what it wrote, fix it, or undo it.

## Who it's for

Developers, consultants, and small teams moving from read-only copilots to
agents that are allowed to act: operations, accounting, approvals, support, and
other work where the data matters. Each store serves one organization. Many
agents and apps can share it.

## Quick start (self-hosted)

Requires Go 1.22 or later.

**1. Install and create secrets.**

```sh
go install github.com/kyle-visner/stellarjay/cmd/stellarjay-server@latest
stellarjay-server init ./secrets
```

`init` creates a data encryption key and prints an admin, writer, and reader
token once. Save them in a password manager.

**2. Start the server.**

```sh
export STELLARJAY_DATA_DIR=./data
export STELLARJAY_DATA_KEY_FILE=./secrets/data_key
export STELLARJAY_AUTH_FILE=./secrets/auth.json
stellarjay-server serve
```

It listens on `127.0.0.1:8080`.

**3. Record a fact.** In another terminal:

```sh
export STELLARJAY_URL=http://127.0.0.1:8080
export STELLARJAY_TOKEN='the-writer-token'

curl -fsS -X POST "$STELLARJAY_URL/v1/events" \
  -H "Authorization: Bearer $STELLARJAY_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: first-fact" \
  --data '{
    "type": "business.fact",
    "entity_id": "customer-42",
    "command": "fact assert",
    "payload": {"predicate": "primary_contact", "value": "Ada Lovelace"},
    "expected_root": ""
  }'
```

`expected_root` is empty only for the first entry in a new store. After that,
read the current value from `GET /v1/root` and send it with each write, so a
write based on stale information is refused. The [API guide](docs/api.md)
covers reading history, pagination, named checkpoints, and snapshots.

**4. Connect an agent.** Point the MCP server at your store:

```sh
go install github.com/kyle-visner/stellarjay/cmd/stellarjay-mcp@latest
```

Then use the self-hosted config from [Agent setup](#agent-setup) with
`STELLARJAY_URL=http://127.0.0.1:8080` and your writer token. Agents that don't
use MCP can read `$STELLARJAY_URL/llm.txt`, which explains how to work with the
store.

## Deploy to a server

For a production store with HTTPS, you need a Linux host with Docker Compose,
ports 80 and 443 open, and a DNS record pointing a domain at the host.

```sh
git clone https://github.com/kyle-visner/stellarjay.git
cd stellarjay
cp .env.example .env
# Edit .env and set STELLARJAY_DOMAIN.

go run ./cmd/stellarjay-server init ./secrets

docker compose up -d --build
curl https://stellarjay.example.com/health/ready
```

`init` will not replace existing secrets. The
[operations runbook](docs/operations.md) covers backups, token rotation, and
upgrades, and the [security model](docs/security.md) covers hardening for
financial and personal data. Or skip all of this and use
[AvianSuite](https://aviansuite.com).

## Documentation

- [llm.md](llm.md): the guide agents follow to read and write safely
- [docs/mcp.md](docs/mcp.md): MCP server setup and tools
- [docs/how-it-works.md](docs/how-it-works.md): storage model, deployment,
  and the embedded Go library
- [docs/api.md](docs/api.md): HTTP API reference ([OpenAPI](docs/openapi.json))
- [docs/architecture.md](docs/architecture.md), [docs/security.md](docs/security.md),
  [docs/operations.md](docs/operations.md): running it in production

## Development

```sh
GOCACHE=/tmp/stellarjay-gocache go test -race ./...
GOCACHE=/tmp/stellarjay-gocache go vet ./...
docker compose config
docker build -t stellarjay:test .
```

## License

AGPL-3.0-or-later. See `LICENSE`. Hosted Stellar Jay is available from
[AvianSuite](https://aviansuite.com).
