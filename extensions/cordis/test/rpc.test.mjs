import test from 'node:test'
import assert from 'node:assert/strict'
import { createServer, connect } from 'node:net'
import { once } from 'node:events'
import { Peer } from '../src/rpc.mjs'
import { HostError } from '../../sdk/index.mjs'

test('JSON-RPC returns public HostError and preserves nested error codes', async t => {
  const server = createServer()
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  t.after(() => server.close())
  const accepted = once(server, 'connection')
  const clientSocket = connect(server.address().port, '127.0.0.1')
  const [serverSocket] = await accepted
  const host = new Peer(serverSocket, async method => {
    if (method === 'lookup') throw new HostError('NOT_FOUND', 'Record not found')
  })
  const plugin = new Peer(clientSocket, async () => { await plugin.call('lookup', {}) })
  t.after(() => { host.close(); plugin.close() })
  await assert.rejects(plugin.call('lookup', {}), error => error instanceof HostError && error.code === 'NOT_FOUND')
  await assert.rejects(host.call('nested', {}), error => error instanceof HostError && error.code === 'NOT_FOUND')
})
