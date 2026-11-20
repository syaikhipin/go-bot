import assert from 'node:assert/strict'
import test from 'node:test'
import { connectionLabel, mcpAuthorizationRunning, pluginOAuthConnections } from '../src/extensions/connections.ts'
import type { MCPServer } from '../src/api/extensions.ts'

const server = (overrides: Partial<MCPServer> = {}): MCPServer => ({ name: 'mail__account', source: 'plugin', state: 'waiting-authorization', oauthEnabled: true, ...overrides })

test('connection choices follow declared OAuth and exact plugin instance namespaces', () => {
 const services = [server(), server({ name: 'mail__public', oauthEnabled: false }), server({ name: 'mailbox__other' }), server({ name: 'mail__manual', source: 'user' })]
 assert.deepEqual(pluginOAuthConnections(services, ['mail']).map(item => item.name), ['mail__account'])
 assert.deepEqual(pluginOAuthConnections(services, []), [])
})

test('credential authorization and an actual service connection remain distinct', () => {
 const grant = { id: 'attempt', name: 'mail__account', state: 'authorized', phase: 'authorized' }
 assert.equal(connectionLabel(server({ state: 'failed', authorization: grant })), 'Credential saved')
 assert.equal(connectionLabel(server({ state: 'connected' })), 'Connected')
 assert.equal(mcpAuthorizationRunning({ ...grant, phase: 'committing' }), true)
 assert.equal(mcpAuthorizationRunning({ ...grant, phase: 'failed' }), false)
 assert.equal(mcpAuthorizationRunning(undefined), false)
})
