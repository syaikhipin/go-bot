import { test } from 'node:test'
import assert from 'node:assert/strict'
import { HostRuntime } from '../src/host-runtime.mjs'

function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

async function fixture(module, overrides = {}) {
  const contributions = new Map(), calls = []
  const runtime = await HostRuntime.create({ directory: '/unused',
    loadPackage: async () => ({ module }),
    call: async (method, input, signal, options) => {
      calls.push({ method, input, options })
      signal?.throwIfAborted()
      if (overrides[method]) return overrides[method](input, signal)
      if (method === 'contribution.register') {
        const id = `go-${contributions.size + 1}`
        contributions.set(id, input)
        return { id }
      }
      if (method === 'contribution.remove') contributions.delete(input.registrationId)
      if (method === 'host.invoke') return { owner: input.owner, key: input.input?.key }
      return null
    },
  })
  return { runtime, contributions, calls }
}

test('async registration waits for Go and binds each Cordis instance to its own owner', async () => {
  const entered = deferred(), host = deferred(), owners = []
  const module = { name: 'mail', inject: ['host'], async apply(ctx) {
    const pending = ctx.host.tools.register({ name: 'send', description: 'Send', inputSchema: { type: 'object' },
      async execute() { return { content: [{ type: 'text', text: 'sent' }] } } })
    assert.equal(typeof pending.then, 'function')
    entered.resolve()
    await pending
    owners.push((await ctx.host.credentials.get('mailbox')).owner)
  } }
  const f = await fixture(module, { 'contribution.register': async () => { await host.promise; return { id: 'registered' } } })
  try {
    const loading = f.runtime.apply({ instances: [{ id: 'work', package: 'mail', enabled: true }] })
    await entered.promise
    assert.equal(f.runtime.inspect()[0].state, 'loading')
    // While one plugin awaits Go, the Node event loop is available.
    await new Promise(resolve => setImmediate(resolve))
    host.resolve()
    await loading
    assert.equal(f.runtime.inspect()[0].state, 'ready')
    assert.deepEqual(owners, ['work'])
    await f.runtime.apply({ instances: [
      { id: 'personal', package: 'mail', enabled: true },
      { id: 'work', package: 'mail', enabled: true },
    ] })
    assert.deepEqual(owners, ['work', 'personal'])
    assert.equal(f.runtime.inspect().filter(row => row.state === 'ready').length, 2)
  } finally { await f.runtime.close() }
})

test('Cordis resolves service dependencies and unregisters only the removed entry', async () => {
  const f = await fixture(null)
  const modules = {
    consumer: { name: 'consumer', inject: ['host', 'mailClient'], async apply(ctx) {
      await ctx.host.tools.register({ name: 'send', description: 'Send', inputSchema: { type: 'object' },
        async execute() { return { content: [{ type: 'text', text: ctx.mailClient.name }] } } })
    } },
    provider: { name: 'provider', apply(ctx) { ctx.provide('mailClient', { name: 'native Cordis service' }) } },
  }
  f.runtime.loadPackage = async name => ({ module: modules[name] })
  try {
    await f.runtime.apply({ instances: [
      { id: 'consumer', package: 'consumer', enabled: true },
      { id: 'provider', package: 'provider', enabled: true },
    ] })
    assert.equal(f.contributions.size, 1)
    assert.equal([...f.contributions.values()][0].owner, 'consumer')
    assert.equal(f.runtime.inspect().filter(row => row.state === 'ready').length, 2)
    await f.runtime.apply({ instances: [{ id: 'provider', package: 'provider', enabled: true }] })
    assert.equal(f.contributions.size, 0)
    assert.deepEqual(f.runtime.inspect().map(row => row.id), ['provider'])
  } finally { await f.runtime.close() }
})

test('tool body is a direct callback and can await a nested Host request', async () => {
  let events = 0
  const f = await fixture({ name: 'nested', inject: ['host'], async apply(ctx) {
    ctx.on('host/skills-changed', () => { events++ })
    await ctx.host.tools.register({ name: 'lookup', description: 'Lookup', inputSchema: { type: 'object' },
      async execute(args, call) {
        assert.equal(call.sessionId, 'session-a')
        const record = await ctx.host.credentials.get(args.key)
        return { content: [{ type: 'text', text: `${record.owner}/${record.key}` }] }
      } })
  } })
  try {
    await f.runtime.apply({ instances: [{ id: 'nested', package: 'nested', enabled: true }] })
    const row = [...f.contributions.values()][0]
    const result = await f.runtime.invoke({ owner: 'nested', callbackId: row.callbackId, method: 'execute',
      args: { key: 'mailbox' }, context: { sessionId: 'session-a', invocation: 'live-go-call' } }, new AbortController().signal)
    assert.equal(events, 0)
    assert.deepEqual(result, { content: [{ type: 'text', text: 'nested/mailbox' }] })
    f.runtime.notify('host/skills-changed', { callId: 'a' })
    assert.equal(events, 1)
  } finally { await f.runtime.close() }
})

test('initialization failure withdraws accepted contributions and cannot report ready', async () => {
  const f = await fixture({ name: 'broken', inject: ['host'], async apply(ctx) {
    await ctx.host.prompts.register({ id: 'hint', location: 'system', text: 'hint' })
    throw new Error('invalid account')
  } })
  try {
    await f.runtime.apply({ instances: [{ id: 'broken', package: 'broken', enabled: true }] })
    assert.equal(f.contributions.size, 0)
    assert.equal(f.runtime.inspect()[0].state, 'failed')
    assert.equal(f.runtime.callbacks.size, 0)
  } finally { await f.runtime.close() }
})

test('Host notification return values and listener failures cannot become tool decisions', async () => {
  const seen = [], warnings = []
  const f = await fixture({ name: 'observer', inject: ['host'], apply(ctx) {
    ctx.on('host/credential-changed', () => { throw new Error('observer failed') })
    ctx.on('host/credential-changed', async () => { seen.push('second'); return { kind: 'deny' } })
  } })
  f.runtime.onLog = record => warnings.push(record)
  try {
    await f.runtime.apply({ instances: [{ id: 'observer', package: 'observer', enabled: true }] })
    assert.equal(f.runtime.notify('host/credential-changed', { outcome: 'success' }), undefined)
    await new Promise(resolve => setImmediate(resolve))
    assert.deepEqual(seen, ['second'])
    assert.equal(warnings.length, 1)
  } finally { await f.runtime.close() }
})

test('Host inputs and callback results reject lossy JSON while authorization run may return void', async () => {
  const f = await fixture({ name: 'json', inject: ['host'], async apply(ctx) {
    await assert.rejects(ctx.host.credentials.put('account', { kind: 'grant', payload: new Date() }), error => error.code === 'INVALID_ARGUMENT')
    await ctx.host.tools.register({ name: 'missing', description: 'Missing return', inputSchema: { type: 'object' }, async execute() {} })
    await ctx.host.tools.register({ name: 'date', description: 'Non JSON return', inputSchema: { type: 'object' }, async execute() { return new Date() } })
    await ctx.host.authorization.register({ key: 'account', title: 'Account', methods: [{ id: 'test', label: 'Test' }], async run() {} })
  } })
  try {
    await f.runtime.apply({ instances: [{ id: 'json', package: 'json', enabled: true }] })
    assert.equal(f.calls.some(({ method, input }) => method === 'host.invoke' && input.service === 'credentials'), false, 'invalid data must not reach Go')
    for (const row of f.contributions.values()) {
      const invoke = () => f.runtime.invoke({ owner: row.owner, callbackId: row.callbackId, method: row.kind === 'authorization' ? 'run' : 'execute', args: {}, context: {} }, new AbortController().signal)
      if (row.kind === 'authorization') assert.equal(await invoke(), null)
      else await assert.rejects(invoke(), error => error.code === 'INVALID_RESULT')
    }
  } finally { await f.runtime.close() }
})

test('disposing during registration removes a late Go acceptance instead of losing its handle', async () => {
  const entered = deferred(), accepted = deferred()
  let scope
  const f = await fixture({ name: 'late', inject: ['host'], async apply(ctx) {
    scope = ctx
    const pending = ctx.host.tools.register({ name: 'late', description: 'Late acceptance', inputSchema: { type: 'object' }, async execute() { return { content: [] } } })
    entered.resolve()
    await pending
  } }, { 'contribution.register': async () => {
    await accepted.promise
    f.contributions.set('late-id', {})
    return { id: 'late-id' }
  } })
  try {
    const loading = f.runtime.apply({ instances: [{ id: 'late', package: 'late', enabled: true }] })
    await entered.promise
    const disposing = scope.fiber.dispose()
    accepted.resolve()
    await Promise.all([loading, disposing])
    assert.equal(f.calls.find(call => call.method === 'contribution.register').options.drain, true)
    assert.equal(f.contributions.size, 0)
    assert.equal(f.runtime.callbacks.size, 0)
  } finally { await f.runtime.close() }
})

test('native authorization inspect/disconnect stay asynchronous and do not invent a Go credential record',async()=>{
 let disconnected=false
 const f=await fixture({name:'native',inject:['host'],async apply(ctx){
  await ctx.host.authorization.register({key:'account',title:'Native account',methods:[{id:'login',label:'Connect'}],async run(){},async inspect({signal}){assert.ok(signal instanceof AbortSignal);return {configured:!disconnected,accountLabel:'alice'}},async disconnect({signal}){assert.ok(signal instanceof AbortSignal);disconnected=true}})
 }})
 try{
  await f.runtime.apply({instances:[{id:'native',package:'native',enabled:true}]})
  const row=[...f.contributions.values()][0]
  assert.deepEqual(row.callbacks.sort(),['disconnect','inspect','run'])
  assert.equal('inspect' in row.descriptor,false,'functions must be callbacks, not serialized JSON')
  const invoke=method=>f.runtime.invoke({owner:row.owner,callbackId:row.callbackId,method,args:{},context:{}},new AbortController().signal)
  assert.deepEqual(await invoke('inspect'),{configured:true,accountLabel:'alice'})
  assert.equal(await invoke('disconnect'),null)
  assert.deepEqual(await invoke('inspect'),{configured:false,accountLabel:'alice'})
  assert.equal(f.calls.some(row=>row.method==='host.invoke'&&row.input.service==='credentials'),false)
 }finally{await f.runtime.close()}
})
