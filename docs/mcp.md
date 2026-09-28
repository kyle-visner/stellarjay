# MCP server

Safe write access for AI agents. The AvianSuite MCP server lets an agent keep
business facts in a Stellar Jay store: every change is kept and attributed to
the agent that made it, and any change can be undone.

## Setup

Hosted (AvianSuite): add `https://mcp.aviansuite.com/mcp` as a remote MCP
server. Clients that support OAuth sign you in; others send an AvianSuite agent
token as `Authorization: Bearer <token>`.

```sh
# Claude Code
claude mcp add --transport http aviansuite https://mcp.aviansuite.com/mcp
```

```json
// Cursor: .cursor/mcp.json
{"mcpServers": {"aviansuite": {"url": "https://mcp.aviansuite.com/mcp"}}}
```

In Claude and ChatGPT, add a custom connector with the same URL.

Self-hosted Stellar Jay: run the stdio server against your store.

```sh
go install github.com/kyle-visner/stellarjay/cmd/stellarjay-mcp@latest
```

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

`STELLARJAY_TOKEN` is a writer token from `stellarjay-server add-token`. Give
each agent its own token so its changes are attributed to it and can be undone
on their own.

## Tools

| Tool | What it does | Writes |
|---|---|---|
| `record_fact` | Save a business fact with its evidence | yes |
| `correct_fact` | Replace an earlier fact; the old one stays in history | yes |
| `retract_fact` | Withdraw a fact with a reason | yes |
| `get_entity` | Current facts and full history for one entity | no |
| `list_changes` | What changed in a time window, by agent or entity | no |
| `undo_changes` | Preview, then reverse everything an agent did in a window | yes |
| `save_checkpoint` | Name the current state, such as `before-import` | yes |
| `status` | Store health and current root | no |

Write tools are marked `destructiveHint: false`: nothing is overwritten or
deleted, so an undo is itself a new change that can be undone.

## How writes work

Facts are `business.fact` events with the payload conventions in
[llm.md](../llm.md): `predicate`, `value`, `evidence`, `observed_at`,
`confidence`, `supersedes`, `retracts` and `reason`. The latest fact in force
for a predicate is current. A correction supersedes the fact it names. A
retraction withdraws its target, and retracting a retraction restores it.

Every write takes an `operation_id`. The server turns it into the store's
`Idempotency-Key`, so a retry of the same write is recorded once, and reusing
an `operation_id` for a different write is refused. The server reads the root
right before each write and retries once if another writer moved it.

Hosts can attach a receipt link to every write (AvianSuite links a page with the
change and an Undo button), and can mark a request read-only, for example when
a workspace has expired: write tools then refuse with the reason, while reads
and undo dry runs keep working.

`undo_changes` is a dry run unless `confirm: true` is passed. It reverses the
actor's fact events in the window, newest first, skips anything already
reversed, and lists events that are not facts (such as AvianSuite table
records) as skipped.
