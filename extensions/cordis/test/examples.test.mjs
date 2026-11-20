import test from 'node:test'
import assert from 'node:assert/strict'
import { HostRuntime } from '../src/host-runtime.mjs'

test('shipped JavaScript examples run with native Cordis configuration and service injection', async t => {
  const contributions = new Map()
  const names = { provider: 'catalog-provider', lookup: 'catalog-tools', mcp: 'mcp-service' }
  const runtime = await HostRuntime.create({ directory: '/unused',
    loadPackage: async name => ({ module: await import(new URL(`../../../examples/plugins/${names[name]}/index.mjs`, import.meta.url)) }),
    call: async (method, input) => {
      if (method === 'contribution.register') { const id = input.owner + '/' + input.callbackId; contributions.set(id, input); return { id } }
      if (method === 'contribution.remove') contributions.delete(input.registrationId)
      return null
    },
  })
  t.after(() => runtime.close())
  const settings = { instances: [
    { id: 'lookup', package: 'lookup', enabled: true, config: { toolName: 'lookup', prefix: 'Found: ' } },
    { id: 'catalog', package: 'provider', enabled: true, config: { entries: { support: 'Contact support' } } },
    { id: 'docs', package: 'mcp', enabled: true, config: { serverName: 'docs', url: 'https://example.com/mcp', apiKey: 'DOCS_KEY' } },
  ] }
  const loaded = await runtime.apply(settings)
  assert.deepEqual(loaded.instances.map(row => row.state), ['ready', 'ready', 'ready'])
  const tool = [...contributions.values()].find(row => row.kind === 'tool')
  const result = await runtime.invoke({ owner: tool.owner, callbackId: tool.callbackId, method: 'execute', args: { key: 'support' }, context: {} }, new AbortController().signal)
  assert.deepEqual(result, { content: [{ type: 'text', text: 'Found: Contact support' }], data: { found: true, text: 'Found: Contact support' } })
  const mcp = [...contributions.values()].find(row => row.kind === 'mcp')
  assert.deepEqual(mcp.descriptor.connection.headers, { Authorization: 'Bearer ${credential:DOCS_KEY}' })
  await runtime.apply({ instances: [] })
  assert.equal(contributions.size, 0)
  const failed = await runtime.apply({ instances: [{ id: 'invalid', package: 'mcp', enabled: true, config: { serverName: 'docs', url: 'not-a-url' } }] })
  assert.equal(failed.instances[0].state, 'failed')
  assert.equal(contributions.size, 0, 'invalid native config must not register an MCP connection')
})
