import { Context } from '@deepseek-ai/cordis'
import Loader from '@deepseek-ai/cordis-plugin-loader'
import { pathToFileURL } from 'node:url'
import { isAbsolute, join, relative, resolve } from 'node:path'
import { realpath, stat } from 'node:fs/promises'
import { randomUUID } from 'node:crypto'
import { HostService, assertJSON, ownerOf, runCallback } from './host-plugin.mjs'
import { readManifest } from './manifest.mjs'
import { RuntimeError } from './errors.mjs'

const hostEvents = new Set(['host/skills-changed', 'host/credential-changed', 'host/authorization-changed', 'host/mcp-changed', 'host/stopping'])
const states = ['pending', 'loading', 'ready', 'failed', 'disposed', 'stopping']
const instanceName = /^[a-z][a-z0-9-]{0,23}$/
const builtinPrefix = 'cordis:go-bot:'

// Cordis owns entries, dependencies, fibers and disposal. The adapter only
// resolves packages and translates calls; it does not own another plugin tree.
export class HostRuntime {
  constructor({ directory, call, loadPackage, onLog = () => {} }) {
    this.directory = directory
    this.call = call
    this.loadPackage = loadPackage ?? (async name => {
      const value = await readManifest(directory, name)
      return { ...value, module: await import(pathToFileURL(value.entry).href) }
    })
    this.onLog = onLog
    this.root = new Context()
    this.callbacks = new Map()
    this.failures = new Map()
    this.closed = false
  }

  static async create(options) {
    const runtime = new HostRuntime(options)
    runtime.root.logger.exporter({ export: options.onLog ?? (() => {}) })
    await runtime.root.plugin(Loader, { baseUrl: pathToFileURL(join(options.directory, 'package.json')).href })
    await runtime.root.plugin({ name: 'go-bot-host-bridge', apply(ctx) { new HostService(ctx, runtime) } })
    return runtime
  }

  remember(ctx, functions) {
    const owner = ownerOf(ctx, this.root.loader)
    if (this.closed) throw new RuntimeError('UNAVAILABLE', 'Plugin runtime is closed')
    const id = randomUUID(), controller = new AbortController()
    this.callbacks.set(id, { ctx, owner, functions, controller })
    ctx.effect(() => () => this.forget(id), 'Host callback lifetime')
    return id
  }

  forget(id) {
    const entry = this.callbacks.get(id)
    if (!entry) return
    this.callbacks.delete(id)
    entry.controller.abort(new RuntimeError('CANCELLED', 'Plugin registration was disposed'))
  }

  async invoke(params, signal) {
    const entry = this.callbacks.get(params.callbackId)
    if (!entry || entry.owner.signal.aborted || (params.owner && params.owner !== entry.owner.id)) throw new RuntimeError('UNAVAILABLE', 'Plugin callback is no longer available')
    const returned = await runCallback(entry, params, AbortSignal.any([signal, entry.controller.signal]))
    const result = returned === undefined && params.method === 'run' ? null : returned
    try { assertJSON(result) }
    catch (error) { throw new RuntimeError('INVALID_RESULT', error.message) }
    return result
  }

  async skillDirectory(ctx, path) {
    if (typeof path !== 'string' || !path) throw new RuntimeError('INVALID_ARGUMENT', 'A Skill directory is required')
    const owner = ownerOf(ctx, this.root.loader)
    const entry = [...this.root.loader.entries()].find(entry => entry.id === owner.id)
    const { root } = await this.loadPackage(entry.options.name.slice(builtinPrefix.length))
    const directory = await realpath(resolve(root, path)), local = relative(root, directory)
    if (local === '..' || local.startsWith('../') || isAbsolute(local) || !(await stat(directory)).isDirectory()) {
      throw new RuntimeError('INVALID_ARGUMENT', 'A Skill directory must be inside the installed package')
    }
    owner.signal.throwIfAborted()
    return directory
  }

  async apply(settings) {
    if (this.closed) throw new RuntimeError('UNAVAILABLE', 'Plugin runtime has closed')
    if (this.applying) throw new RuntimeError('CONFLICT', 'A plugin configuration is already being applied')
    this.applying = true
    try {
      const rows = settings.instances ?? [], ids = new Set(), desired = []
      this.failures.clear()
      for (const row of rows) {
        if (!instanceName.test(row.id) || ids.has(row.id)) throw new RuntimeError('INVALID_ARGUMENT', 'Invalid or duplicate plugin instance id')
        ids.add(row.id)
        const name = builtinPrefix + row.package
        if (row.enabled) {
          try { this.root.loader.builtins[name.slice(7)] = (await this.loadPackage(row.package)).module }
          catch (error) { this.failures.set(row.id, { package: row.package, error: String(error?.message ?? error) }); continue }
        }
        desired.push({ id: row.id, name, disabled: !row.enabled, config: row.config ?? {} })
      }
      const loader = this.root.loader
      const previous = [...loader.entries()].map(entry => entry.fiber).filter(Boolean)
      await loader.root.update(desired)
      await loader.await()
      // Removed entries no longer appear in Loader.await(). Join Cordis's
      // actual cleanup promise rather than tracking our own disposal states.
      await Promise.all(previous.map(fiber => fiber.inertia))
      for (const entry of loader.entries()) {
        try { await entry.fiber?.await() }
        catch (error) { this.failures.set(entry.id, { package: entry.options.name.slice(builtinPrefix.length), error: String(error?.message ?? error) }) }
      }
      return { instances: this.inspect(), warnings: [] }
    } finally { this.applying = false }
  }

  inspect() {
    const result = [...this.root.loader.entries()].map(entry => ({
      id: entry.id, package: entry.options.name.slice(builtinPrefix.length), name: entry.options.name.slice(builtinPrefix.length),
      state: entry.disabled ? 'disabled' : states[entry.fiber?.state] ?? 'failed',
    }))
    for (const [id, failure] of this.failures) {
      const existing = result.find(row => row.id === id)
      if (existing) Object.assign(existing, { state: 'failed', error: failure.error })
      else result.push({ id, package: failure.package, name: failure.package, state: 'failed', error: failure.error })
    }
    return result
  }

  notify(name, data) {
    if (!hostEvents.has(name) || this.closed) return
    const value = Object.freeze(structuredClone(data))
    for (const callback of this.root.events.dispatch('emit', [name, value])) {
      try { Promise.resolve(callback(value)).catch(error => this.onLog({ type: 'warn', args: [name, String(error?.message ?? error)] })) }
      catch (error) { this.onLog({ type: 'warn', args: [name, String(error?.message ?? error)] }) }
    }
  }

  async close() {
    if (this.closing) return this.closing
    this.closing = (async () => {
      await this.root.fiber.dispose()
      this.closed = true
    })()
    return this.closing
  }
}
