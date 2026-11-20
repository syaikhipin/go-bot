# Upstream source oracles

These optional differential checks compare the Go built-in MCP OAuth and model
selection behavior with pinned upstream source modules. They do not establish
compatibility with arbitrary upstream Cordis/npm plugins.

Business plugins use the project's own asynchronous `ctx.host` SDK. See
[`extensions/sdk`](../../extensions/sdk/README.md) and
[`examples/plugins`](../../examples/plugins/README.md). The production runtime
uses native Cordis/Loader for lifecycle; Go owns the Agent, Session, tools,
approval, credentials and MCP. There are no plugin tool-stage middleware or
guard interfaces.

## MCP OAuth source interoperability

The pinned upstream source revision is
`ca33fd6843b205e279c7ccb6cb73a472c78f5760`. Its
`packages/mcp/mcp-client/src/oauth-credentials.ts` contains newer OAuth support
than the published `@deepseek-ai/dsh-mcp-client@0.2.2-rc.4` npm artifact. Do not
substitute that older artifact when validating this contract.

With the unedited source module compiled to ESM and its original dependencies
installed, run this opt-in differential test (use an absolute module path):

```sh
GO_BOT_UPSTREAM_MCP_OAUTH_MODULE=/absolute/path/to/upstream-credentials.mjs \
  go test -count=1 -v ./internal/mcpclient \
    -run '^TestOAuthRecordsInteroperateWithPinnedUpstream$'
```

The test runs real local OAuth/MCP servers. Go performs authorization and writes
the shared credential records; the original upstream class reads those records
and refreshes through its SDK; Go then reconnects using the upstream-written
record. It covers dynamic public, dynamic confidential and configured public
clients, without calling a paid model or third-party account. The helper
`mcp-oauth-oracle.mjs` is test code, not a second production credential provider.

## Agent model-selection source oracle

At the same pinned source revision, compile the unedited
`packages/core/agent/src/model-selection.ts` to ESM with its original runtime
dependencies available. Regenerate the projection and AgentModel expectations:

```sh
node tests/upstream-plugins/model-selection-oracle.mjs \
  /absolute/path/to/upstream-model-selection.mjs \
  < harness/session/testdata/model-selection-cases.json \
  > /tmp/model-selection-upstream.json
diff -u harness/session/testdata/model-selection-upstream.json /tmp/model-selection-upstream.json
go test -count=1 ./harness/session -run 'TestModelSelectionMatches|TestConcurrentModelSelect'
```

The 19 actions cover live fallback, explicit selection, deduplication, pending
selection retirement, null selection, changed request routes and adapter-defaulted
effort. The ordinary Go test uses the checked-in source-generated expectations
and reopens an actual JSONL journal. The helper's minimal append recorder is
test-only; production continues using the existing Go Session.


Core Agent/Session class compatibility, synchronous append and factory lifecycle
oracles are intentionally excluded. Corresponding behavior is tested inside Go;
the plugin boundary is tested through real tool/Skill/MCP/auth contributions.
