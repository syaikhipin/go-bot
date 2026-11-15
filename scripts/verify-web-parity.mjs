// Recompute fixture results from the pinned, unmodified DeepSeek Harness tools.
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { copyFileSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const [, , checkoutArg, flag] = process.argv
if (!checkoutArg || (flag !== undefined && flag !== '--write')) {
  throw new Error('usage: node scripts/verify-web-parity.mjs UPSTREAM_CHECKOUT [--write]')
}
const root = dirname(dirname(fileURLToPath(import.meta.url)))
const checkout = resolve(checkoutArg)
const pin = 'fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159'
const fixtures = join(root, 'harness/tools/testdata/web-parity.json')
const stage = mkdtempSync(join(tmpdir(), 'harness-web-parity-'))
const source = path => execFileSync('git', ['-C', checkout, 'show', `${pin}:${path}`], {encoding: 'utf8'})
try {
  for (const name of ['package.json', 'package-lock.json']) {
    copyFileSync(join(root, 'build/web-markdown', name), join(stage, name))
  }
  execFileSync('npm', ['ci', '--ignore-scripts', '--no-audit', '--no-fund'], {cwd: stage, stdio: ['ignore', 2, 2]})
  for (const name of ['search', 'fetch', 'trust']) {
    writeFileSync(join(stage, name + '.ts'), source(`packages/web/tool-web/src/${name}.ts`))
  }
  const retention = source('packages/util/output-retention/src/index.ts')
  const truncate = retention.match(/export function truncateWithoutSplittingSurrogatePair\([\s\S]*?\n}/)
  assert.ok(truncate)
  writeFileSync(join(stage, 'tool-facade.ts'), `
    export const defineTool = value => value
    export function assertNever() { throw new Error('unhandled fetch body') }
    ${truncate[0]}
  `)
  writeFileSync(join(stage, 'entry.ts'), `
    export {applyWebSearchTool} from './search.ts'
    export {applyWebFetchTool} from './fetch.ts'
  `)
  execFileSync('go', ['run', 'github.com/evanw/esbuild/cmd/esbuild', join(stage, 'entry.ts'),
    '--bundle', '--platform=node', '--format=esm', '--outfile=' + join(stage, 'reference.mjs'),
    ...['dsh-tools', 'dsh-util-values', 'dsh-output-retention'].map(
      name => `--alias:@deepseek-ai/${name}=${join(stage, 'tool-facade.ts')}`),
  ], {cwd: root, stdio: ['ignore', 2, 2]})
  const { applyWebSearchTool, applyWebFetchTool } = await import(pathToFileURL(join(stage, 'reference.mjs')).href)
  const input = JSON.parse(readFileSync(fixtures, 'utf8'))
  const cases = []
  for (const test of input.cases) {
    let tool
    const ctx = {
      tools: { register(value) { tool = value } },
      systemPrompt: { section() {}, getSectionOrder() { return 0 } },
      web: {
        async search({query, maxResults}) {
          assert.equal(maxResults, 10)
          const result = test.search[query]
          return result.sources.length > maxResults
            ? {...result, sources: result.sources.slice(0, maxResults), truncated: true}
            : result
        },
        async fetch({url}) {
          assert.equal(url, test.args.url)
          return test.fetch
        },
      },
    }
    if (test.tool === 'web_search') applyWebSearchTool(ctx, 10, 10, 800, 60_000, true)
    else applyWebFetchTool(ctx, 30_000, 200_000)
    const {expected: prior, ...fixture} = test
    let expected
    try {
      const value = await tool.execute(test.args, {signal: new AbortController().signal})
      expected = {value, text: tool.output.render(test.args, value)[0].text, meta: tool.output.presentationMeta(test.args, value)}
    } catch (err) {
      expected = {error: err.message, text: `Error: ${err.message}`}
    }
    if (flag !== '--write') assert.deepEqual(expected, prior, test.name)
    cases.push({...fixture, expected})
  }
  if (flag === '--write') writeFileSync(fixtures, JSON.stringify({upstream: pin, cases}, null, 2) + '\n')
  console.log(`DeepSeek Harness fixtures verified: ${cases.length} cases`)
} finally {
  rmSync(stage, {recursive: true, force: true})
}
