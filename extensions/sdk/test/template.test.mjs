import test from 'node:test'
import assert from 'node:assert/strict'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { mkdtemp, rm, writeFile, readFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { HostRuntime } from '../../cordis/src/host-runtime.mjs'

const run = promisify(execFile)
const sdk = fileURLToPath(new URL('../', import.meta.url))
const repository = fileURLToPath(new URL('../../../', import.meta.url))

test('the TypeScript example runs as JavaScript through native Cordis and Promise Host registration', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'go-bot-sdk-'))
  t.after(() => rm(directory, { recursive: true, force: true }))
  await writeFile(join(directory, 'package.json'), '{"type":"module","private":true}')
  await run(process.execPath, [join(sdk, 'node_modules/typescript/lib/tsc.js'), '-p', join(sdk, 'tsconfig.json'),
    '--noEmit', 'false', '--declaration', '--outDir', directory, '--rootDir', repository])
  const entry = join(directory, 'examples/plugins/typescript-tool/src/index.js')
  assert.ok(!(await readFile(entry, 'utf8')).includes('@go-bot/plugin-sdk'), 'type-only SDK imports leave no deployment dependency')
  const module = await import(pathToFileURL(entry).href)
  const contributions = new Map()
  const runtime = await HostRuntime.create({ directory, loadPackage: async () => ({ module }), call: async (method, input) => {
    if (method === 'contribution.register') {
      const id = input.owner + '/' + input.callbackId
      contributions.set(id, input)
      return { id }
    }
    if (method === 'contribution.remove') contributions.delete(input.registrationId)
    return null
  } })
  t.after(() => runtime.close())
  const loaded = await runtime.apply({ instances: [{ id: 'first', package: 'example', enabled: true }, { id: 'second', package: 'example', enabled: true }] })
  assert.deepEqual(loaded.instances.map(row => row.state), ['ready', 'ready'])
  assert.equal(contributions.size, 2)
  const row = [...contributions.values()][0]
  const result = await runtime.invoke({ owner: row.owner, callbackId: row.callbackId, method: 'execute', args: { text: 'Real compiled plugin' },
    context: { sessionId: 'session', cwd: '/root/harness', workspace: '/root/harness', invocation: 'current-body' } }, new AbortController().signal)
  assert.deepEqual(result, { content: [{ type: 'text', text: 'Real compiled plugin' }], data: { text: 'Real compiled plugin' } })
  await runtime.apply({ instances: [] })
  assert.equal(contributions.size, 0)
})
