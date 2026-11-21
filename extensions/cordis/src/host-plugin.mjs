import { Service } from '@deepseek-ai/cordis'
import { AsyncLocalStorage } from 'node:async_hooks'
import { RuntimeError } from './errors.mjs'

const invocation = Symbol('go-bot.invocation')
const callbackContext = new AsyncLocalStorage()
const lifetimes = new WeakMap()
const fail = (code, message) => { throw new RuntimeError(code, message) }
const managementSignal = (...signals) => AbortSignal.any([...signals.filter(Boolean), AbortSignal.timeout(30_000)])

export function assertJSON(value, ancestors = new Set(), depth = 0) {
  if (depth > 64) fail('INVALID_ARGUMENT', 'JSON nesting exceeds 64 levels')
  if (value === null || typeof value === 'string' || typeof value === 'boolean') return
  if (typeof value === 'number' && Number.isFinite(value)) return
  if (!value || typeof value !== 'object' || ancestors.has(value) ||
      (!Array.isArray(value) && ![Object.prototype, null].includes(Object.getPrototypeOf(value)))) fail('INVALID_ARGUMENT', 'Expected lossless JSON data')
  if (Reflect.ownKeys(value).some(key => typeof key === 'symbol')) fail('INVALID_ARGUMENT', 'Symbols are not JSON data')
  ancestors.add(value)
  for (const item of Array.isArray(value) ? value : Object.values(value)) assertJSON(item, ancestors, depth + 1)
  ancestors.delete(value)
}

export function ownerOf(ctx, loader) {
  const id = loader.locate(ctx.fiber)
  if (!id || [3, 4, 5].includes(ctx.fiber.state)) fail('UNAVAILABLE', 'An active plugin instance is required')
  let controller = lifetimes.get(ctx.fiber)
  if (!controller || controller.signal.aborted) {
    controller = new AbortController()
    lifetimes.set(ctx.fiber, controller)
    ctx.effect(() => () => controller.abort(new RuntimeError('CANCELLED', 'The Cordis plugin scope was disposed')), 'Host requests')
  }
  return { id, signal: controller.signal }
}

export class HostService extends Service {
  constructor(ctx, runtime) { super(ctx, 'host'); this.runtime = runtime }

  async invoke(ctx, service, method, input = {}, { signal, call } = {}) {
    const owner = ownerOf(ctx, this.runtime.root.loader)
    if (call && !call[invocation]) fail('INVALID_ARGUMENT', 'A live tool invocation is required')
    assertJSON(input)
    const current = callbackContext.getStore()
    const signals = [owner.signal, signal, current?.signal].filter(Boolean)
    // Human decisions have their own Go lifecycle, not a management deadline.
    const waitingForUser = service === 'approvals' || service === 'questions' ||
      service === 'authorization' && (method === 'prompt' || method === 'waitCallback')
    return this.runtime.call('host.invoke', {
      owner: owner.id, service, method, input,
      invocation: call?.[invocation], mutation: current?.mutation, discovery: current?.discovery,
    }, waitingForUser ? AbortSignal.any(signals) : managementSignal(...signals))
  }

  info() { return this.invoke(this.ctx, 'info', 'get') }

  get proactive() {
    const ctx = this.ctx
    return { changed: () => this.invoke(ctx, 'proactive', 'changed') }
  }

  get tools() {
    const ctx = this.ctx
    return {
      register: definition => {
        if (typeof definition?.execute !== 'function') fail('INVALID_ARGUMENT', 'A tool needs execute(args, call)')
        const { execute, ...descriptor } = definition
        return this.register(ctx, 'tool', descriptor, { execute })
      },
    }
  }

  get skills() {
    const ctx = this.ctx
    return {
      register: skill => this.register(ctx, 'skill', skill),
      registerDirectory: async path => this.register(ctx, 'skill-directory', { path: await this.runtime.skillDirectory(ctx, path) }),
      registerProvider: provider => {
        if (typeof provider?.list !== 'function' || typeof provider?.get !== 'function') fail('INVALID_ARGUMENT', 'A Skill provider needs list and get')
        return this.register(ctx, 'skill-provider', { id: provider.id }, { list: provider.list, get: provider.get })
      },
      list: (options = {}) => this.invoke(ctx, 'skills', 'list', options),
      get: (name, options = {}) => this.invoke(ctx, 'skills', 'get', { ...options, name }),
      invalidate: () => this.invoke(ctx, 'skills', 'invalidate'),
    }
  }

  get prompts() {
    const ctx = this.ctx
    return { register: definition => {
      const { text, ...descriptor } = definition
      if (typeof text !== 'string' && typeof text !== 'function') fail('INVALID_ARGUMENT', 'A prompt needs text')
      return this.register(ctx, 'prompt', typeof text === 'string' ? { ...descriptor, text } : descriptor,
        typeof text === 'function' ? { text } : {})
    } }
  }

  get credentials() {
    const ctx = this.ctx
    const request = (method, input) => this.invoke(ctx, 'credentials', method, input)
    return {
      get: key => request('get', { key }), put: (key, record) => request('put', { key, record }),
      remove: key => request('remove', { key }), describe: key => request('describe', { key }),
      list: () => request('list', {}), resolve: ref => request('resolve', { ref }),
      update: async (key, mutate) => {
        if (typeof mutate !== 'function') fail('INVALID_ARGUMENT', 'A credential update needs a callback')
        const callback = this.runtime.remember(ctx, { mutate })
        try { return await request('update', { key, callback }) }
        finally { this.runtime.forget(callback) }
      },
    }
  }

  get authorization() {
    const ctx = this.ctx
    return {
      register: flow => {
        if (typeof flow?.run !== 'function') fail('INVALID_ARGUMENT', 'An authorization flow needs run(session)')
        const { run, inspect, disconnect, title, ...descriptor } = flow
        const optional = {}
        if (inspect && typeof inspect !== 'function' || disconnect && typeof disconnect !== 'function') fail('INVALID_ARGUMENT', 'Authorization inspect and disconnect must be callbacks')
        if (inspect) optional.inspect = (_, context) => inspect({ signal: context.signal })
        if (disconnect) optional.disconnect = async (_, context) => { await disconnect({ signal: context.signal }); return null }
        return this.register(ctx, 'authorization', { ...descriptor, title }, { ...optional, run: async (input, context) => {
          const request = (method, data) => this.invoke(ctx, 'authorization', method,
            { id: input.id, ...data }, { signal: context.signal })
          return run(Object.freeze({
            method: input.method, signal: context.signal,
            notify: notice => request('notify', { notice }),
            prompt: prompt => request('prompt', { prompt }),
            callback: async ({ state }) => {
              const url = await request('callback', { state })
              return Object.freeze({ url, wait: () => request('waitCallback', {}) })
            },
            commit: record => request('commit', { record }),
          }))
        } })
      },
      list: () => this.invoke(ctx, 'authorization', 'list'),
      cancel: key => this.invoke(ctx, 'authorization', 'cancel', { key }),
    }
  }

  get mcp() {
    const ctx = this.ctx
    const request = (method, input = {}) => this.invoke(ctx, 'mcp', method, input)
    return {
      register: definition => this.register(ctx, 'mcp', definition),
      list: () => request('list'), inspect: name => request('inspect', { name }),
      restart: name => request('restart', { name }), authorize: name => request('authorize', { name }),
      cancelAuthorization: (name, attemptId) => request('cancelAuthorization', { name, attemptId }),
      signOut: name => request('signOut', { name }),
    }
  }

  get approvals() {
    const ctx = this.ctx
    return { request: (call, request) => this.invoke(ctx, 'approvals', 'request', request, { call, signal: call.signal }) }
  }

  get questions() {
    const ctx = this.ctx
    return { ask: (call, request) => this.invoke(ctx, 'questions', 'ask', request, { call, signal: call.signal }) }
  }

  async register(ctx, kind, descriptor, functions = {}) {
    const owner = ownerOf(ctx, this.runtime.root.loader)
    assertJSON(descriptor)
    const id = this.runtime.remember(ctx, functions)
    let registrationId, closed = false, cleanup
    const dispose = () => cleanup ??= (async () => {
      closed = true
      try {
        if (registrationId && !this.runtime.closed) await this.runtime.call('contribution.remove', { owner: owner.id, registrationId }, managementSignal())
      } finally { this.runtime.forget(id) }
    })()
    ctx.effect(() => dispose, `ctx.host.${kind} registration`)
    try {
      const signal = managementSignal(owner.signal)
      const registered = await this.runtime.call('contribution.register', { owner: owner.id, callbackId: id, kind,
        descriptor, callbacks: Object.keys(functions) }, signal, { drain: true })
      if (typeof registered?.id !== 'string' || !registered.id) fail('INVALID_RESULT', 'Host registration has no id')
      registrationId = registered.id
      if (closed || signal.aborted) {
        await this.runtime.call('contribution.remove', { owner: owner.id, registrationId }, managementSignal())
        fail('CANCELLED', 'The plugin closed during registration')
      }
      return Object.freeze({ id: registrationId, dispose })
    } catch (error) { this.runtime.forget(id); throw error }
  }
}

export function invocationContext(context, signal) {
  const { invocation: token, mutation: _, discovery: __, ...view } = context ?? {}
  return Object.freeze({ ...view, signal, ...(token ? { [invocation]: token } : {}) })
}

export async function runCallback(entry, params, signal) {
  const callback = entry.functions[params.method]
  if (!callback) fail('NOT_FOUND', 'Plugin callback is unavailable')
  const combined = AbortSignal.any([signal, entry.owner.signal])
  combined.throwIfAborted()
  const context = invocationContext(params.context, combined)
  return callbackContext.run({ mutation: params.context?.mutation, discovery: params.context?.discovery, signal: combined }, async () => {
    if (params.method === 'text' || params.method === 'list') return callback(Object.freeze({ ...params.args, signal: combined }))
    if (params.method === 'get') return callback(params.args.key, Object.freeze({ ...params.args.context, signal: combined }))
    return callback(params.args, context)
  })
}
