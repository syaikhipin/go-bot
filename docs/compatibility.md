# DeepSeek Harness compatibility

This is an independent Go implementation, not an official DeepSeek product.
The migrated native v4 persistence fixtures and strict/current replay verification
use upstream commit `d743267388641bc76f17c45ce8b4c231aed1d32c`.

`web_search` and `web_fetch` follow the shared product defaults at
`fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159`. Search uses DeepSeek Search API and
fetch uses local public HTTP. See the [web tool parity record](web-tools-parity-plan.zh-CN.md)
for reference fixtures, Host-specific boundaries and verification commands.

The runtime preserves v4 framing, typed events, projection, semantic checkpoints,
inbox, interrupted-turn repair, compaction, tools, child lineage, goals, plans,
questions, jobs and schedules. HTTP deployment and controller state are project
adapters outside the upstream Session header.

The Controller stores account settings and lifecycle state in Redis. The Host
keeps its sessions on the filesystem, uses the model selected for the current
session, and authenticates internal HTTP requests with its Host token.
The retired v1 session package exists only under internal/testsupport for migrated
test doubles; production binaries must not depend on it.

Tests using historical fixtures preserve their original data representation.
No user conversation, production credential or deployment log is part of the
intended distribution. Run `scripts/verify-upstream.mjs` against a pinned,
separately installed upstream checkout to validate generated JSONL with the
original strict decoder; upstream TypeScript is not a runtime dependency.

## Main-chat initialization

Go Bot adds a narrowly validated constructor prefix: `system/message` at sequence
0 from `go-bot/initial-system`, followed by a text-only `assistant/message` at
sequence 1 from `go-bot/welcome`, before any execution turn. The welcome has an
empty stream and no usage. These are ordinary model-visible messages and use
native append/replace and compaction; regular model/tool messages still require
an open turn and step. This product initialization is an intentional addition to
the upstream constructor acceptance rules, with no custom v4 Header fields or
special client content blocks.
