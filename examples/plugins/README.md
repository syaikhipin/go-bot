# Plugin examples

These examples use the new Host Bridge SDK (`goBot.api: "1"`). They run as
trusted JavaScript packages in Cordis. Go owns all Harness core execution.

- **typescript-tool**: a text/JSON tool, compiled to JavaScript. Install the local
  SDK development dependency and run `pnpm build` in this directory first.
- **catalog-provider**: a normal Cordis service with no Go-specific business enum.
- **catalog-tools**: injects that service and `host`, then awaits tool registration.
- **mcp-service**: registers an HTTP MCP declaration; Go connects and executes it.
  Its optional `apiKey` config names an existing Go credential reference, never a
  literal key. The plugin does not run a Node MCP client or provide an MCP UI.

Packages can be installed from a registry, a pinned Git revision, a tarball or an
absolute Host development directory. Local paths refer to the Host container,
not the workstation running the browser.

The following is the **Host-private development contract** used in tests to exercise
Cordis entries with custom example configuration. It is not Redis data or a public
user API. Normal users manage one catalog package selection (version/enabled),
with no instance or arbitrary-config form:

```json
{
  "packages": {
    "@go-bot-example/catalog-provider": {
      "source": { "type": "directory", "spec": "/opt/examples/catalog-provider" }
    },
    "@go-bot-example/catalog-tools": {
      "source": { "type": "directory", "spec": "/opt/examples/catalog-tools" }
    }
  },
  "instances": [
    { "id": "catalog", "package": "@go-bot-example/catalog-provider", "enabled": true,
      "config": { "entries": { "support": "Contact the support team" } } },
    { "id": "lookup", "package": "@go-bot-example/catalog-tools", "enabled": true,
      "config": { "toolName": "catalog_lookup", "prefix": "" } }
  ]
}
```

Cordis resolves native dependencies and retracts effects on removal. Each Host
tool is namespaced by instance ID; plugins provide a business `execute` callback,
not a pre/execute/post middleware chain. Do not add actions, storage, Asset RPC,
Agent/Session proxies, client slots or an upstream bundle/patch file.
