// Optional source-level oracle. The module argument must be compiled, unedited
// oauth-credentials.ts from the pinned DeepSeek Harness source revision.
// npm rc.4's MCP package predates this OAuth implementation; do not silently
// substitute that older published package and claim to have checked OAuth.
import {readFileSync} from 'node:fs'
import {pathToFileURL} from 'node:url'

const {McpOAuthCredentials} = await import(pathToFileURL(process.argv[2]).href)
const input = JSON.parse(readFileSync(0, 'utf8'))
const records = structuredClone(input.records)
const provider = {
  async readRecord(key) { return structuredClone(records[key]) },
  async modifyRecord(key, mutate) {
    const next = await mutate(structuredClone(records[key]))
    if (next !== undefined) records[key] = structuredClone(next)
    return structuredClone(records[key])
  },
}
const credentials = new McpOAuthCredentials({get: name => name === 'credentials' ? provider : undefined}, {
  credentialKey: input.key, ...input.clientId ? {clientId: input.clientId} : {},
  ...input.scope !== undefined ? {scope: input.scope} : {}, requestTimeoutMs: 5000, authorizationTimeoutMs: 10000,
})
const signal = AbortSignal.timeout(10000)
const bound = await credentials.read(input.url, signal)
if (bound.tokens.access_token !== input.accessToken) throw new Error('Upstream did not read the Go grant')
const client = await credentials.client(bound.clientKey, bound.issuer)
if (client?.client_id !== bound.clientId) throw new Error('Upstream did not read the Go client record')
records[input.key].payload.expiresAt = 0
const expired = await credentials.read(input.url, signal)
const transport = credentials.transport(expired, signal, globalThis.fetch)
const token = await transport.authProvider.token()
if (!token || token === input.accessToken) throw new Error('Actual upstream refresh did not rotate the token')
process.stdout.write(JSON.stringify({records, refreshed: true}))
