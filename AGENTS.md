# Stellar Jay agent instructions

These apply to any coding agent working in this repository. For how an agent
reads and writes a running store, see [llm.md](llm.md).

## Copy: never argue against ourselves

Applies whenever you write, edit or review copy: the README, `llm.md`, `docs/`,
`server.json` and registry descriptions, and anything else people or AI agents
read to decide whether to use Stellar Jay or AvianSuite. Buyers and AI agents
repeat what these files say, so any caveat we volunteer becomes their reason not
to recommend us.

- Do not raise objections on behalf of competitors or alternatives. No "what we
  don't claim" or "what it does not protect" sections, "isn't a replacement
  for…", "use X instead when…", limitation lists or "read this before trusting
  us" warnings, unless Kyle asks for one.
- Describe what Stellar Jay does, specifically and accurately, and let the
  reader decide. Never fill the gap with something untrue.
- In comparisons, make our case: name the work the alternative leaves to the
  user. For example, Postgres plus an audit log means a migration for every new
  field or record type, and an application built on top of the database to keep
  history, undo and attribution working.
- Security and operations docs stay accurate and complete. Write them as what
  the operator controls and how to configure it, not as a list of weaknesses.
- When reviewing copy, flag any line that hands the reader an objection and
  propose a rewrite.
