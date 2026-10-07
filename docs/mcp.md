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

No AvianSuite account yet? The agent registers with
[auth.md](https://aviansuite.com/auth.md) using the person's email. The person
opens the link it gets, types the six-digit code, confirms their email, and
sets a password to create a free 7-day sandbox (no card); the agent then gets
an access token for
`https://mcp.aviansuite.com/mcp`.

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
| `record_fact` | Save a business fact with its evidence, including a value that has changed since | yes |
| `correct_fact` | Replace a fact that was wrong when recorded; the old one stays in history | yes |
| `retract_fact` | Withdraw a fact with a reason | yes |
| `get_entity` | Current facts and full history for one entity | no |
| `list_changes` | What changed in a time window, by agent or entity | no |
| `undo_changes` | Preview, then reverse everything an agent did in a window, or restore a checkpoint | yes |
| `save_checkpoint` | Name the current state, such as `before-import`, to restore with `undo_changes` | yes |
| `status` | Store health, current root, whether it holds any facts, and the caller's actor name | no |

Every tool declares `readOnlyHint`, `destructiveHint` and `openWorldHint`.
Fact writes are marked `destructiveHint: false`: nothing is overwritten or
deleted, so an undo is itself a new change that can be undone.
`save_checkpoint` is marked `destructiveHint: true`, because saving an
existing name moves that checkpoint to the current state.

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
and undo dry runs keep working. Hosts that reach the store over an internal
address can also set the address `status` reports as `store`, or leave it out.

`undo_changes` is a dry run unless `confirm: true` is passed. It reverses the
actor's fact events in the window, newest first, skips anything already
reversed, and lists events that are not facts (such as AvianSuite table
records) as skipped.

The dry run returns the absolute `since` and `until` it used, and a `plan_id`
that names exactly the changes it would reverse. A confirmed call must pass
those `since` and `until` values (a relative `since` such as `1h` is refused,
because it would move between the two calls) and should pass the `plan_id`;
if the changes in the window are no longer the ones previewed, it refuses and
writes nothing. The confirmed result's `root` is the store after the
reversals, and `root_before` is the root the plan was made against.

To restore a checkpoint saved with `save_checkpoint`, pass `checkpoint` instead
of `since`. That reverses every fact event after the checkpoint, from every
actor unless `actor` is given, so the facts read as they did at the checkpoint.
The dry run returns `checkpoint_root`, `until` and `plan_id`; the confirmed call
passes the same `checkpoint` with that `until` and `plan_id`. When an undo
reverses a retraction, it also reverses the fact that retraction withdrew if
that fact is in the same undo, so a fact added and retracted inside the window
stays gone.

`status` reports `empty: true` until the store holds a business fact, so
setup events such as a workspace being initialized do not count. Hosts can
name the caller's actor (with `mcp.WithActor`), and `status` then returns it as
`actor`, which is the value `list_changes` and `undo_changes` take.
