import { connect } from 'node:net'
import { HostRuntime } from './host-runtime.mjs'
import { MAX_FRAME, Peer } from './rpc.mjs'
import { RuntimeError } from './errors.mjs'

const path = process.argv[2]
if (!path || process.argv[3]) throw new Error('One private Host Unix socket path is required')
const socket = connect(path)
let runtime
const peer = new Peer(socket, async (method, params, signal) => {
  const active = await ready
  switch (method) {
    case 'runtime.apply': return active.apply(params)
    case 'runtime.inspect': return active.inspect()
    case 'callback.invoke': return active.invoke(params, signal)
    case 'host.event': active.notify(params.name, params.data); return null
    case 'runtime.shutdown': active.notify('host/stopping', { reason: 'Host is stopping' }); await active.close(); return null
    default: throw new RuntimeError('NOT_FOUND', 'Unknown plugin runtime method')
  }
})

const ready = (async () => {
  const initialized = await peer.call('bridge.initialize', { protocol: 1, api: '1' })
  const services = ['info', 'tools', 'skills', 'prompts', 'credentials', 'authorization', 'mcp', 'approvals', 'questions']
  if (initialized?.protocol !== 1 || initialized?.api !== '1' || initialized.maxFrameBytes !== MAX_FRAME ||
      !Array.isArray(initialized.capabilities) || services.some(name => !initialized.capabilities.includes(name))) {
    throw new RuntimeError('INCOMPATIBLE_PLUGIN', 'Unsupported Host protocol or capabilities')
  }
  runtime = await HostRuntime.create({ directory: initialized.directory,
    call: (method, input, signal, options) => peer.call(method, input, signal, options),
    onLog: record => peer.notify('runtime.log', { level: record.type, message: record.args.map(String).join(' ') }).catch(() => {}),
  })
  return runtime
})()
ready.catch(() => peer.close())
socket.once('close', () => { void runtime?.close().catch(() => {}) })
process.once('SIGTERM', () => peer.close())
process.once('SIGINT', () => peer.close())
