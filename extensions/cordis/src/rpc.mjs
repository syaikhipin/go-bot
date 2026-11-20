import { RuntimeError } from './errors.mjs'

export const MAX_FRAME = 1 << 20
const classifiedRemoteErrors = new WeakSet()

export class Peer {
  #buffer = Buffer.alloc(0)
  #pending = new Map()
  #active = new Map()
  #sequence = 0
  #closed = false

  constructor(socket, handler) {
    this.socket = socket
    this.handler = handler
    socket.on('data', chunk => this.#read(chunk))
    socket.on('close', () => this.close())
    socket.on('error', error => this.close(error))
  }

  async call(method, params, signal, { drain = false } = {}) {
    if (this.#closed) throw new Error('Host connection closed')
    signal?.throwIfAborted()
    const id = `n${++this.#sequence}`
    return new Promise((resolve, reject) => {
      let drainTimer
      const cancel = () => {
        this.#send({ jsonrpc: '2.0', method: 'request.cancel', params: { id } }).catch(() => {})
        if(drain){
          drainTimer=setTimeout(()=>this.close(new Error('Go capability did not settle after cancellation')),6000)
          return
        }
        this.#pending.get(id)?.cleanup()
        this.#pending.delete(id)
        signal?.removeEventListener('abort', cancel)
        reject(signal.reason)
      }
      this.#pending.set(id, { resolve, reject, cleanup: () => {signal?.removeEventListener('abort', cancel);clearTimeout(drainTimer)} })
      signal?.addEventListener('abort', cancel, { once: true })
      this.#send({ jsonrpc: '2.0', id, method, params }).catch(error => {
        const pending = this.#pending.get(id)
        if (pending) { this.#pending.delete(id); pending.cleanup(); reject(error) }
      })
    })
  }

  notify(method, params) { return this.#send({ jsonrpc: '2.0', method, params }) }

  async #send(message) {
    if (this.#closed) throw new Error('Host connection closed')
    const bytes = Buffer.from(JSON.stringify(message) + '\n')
    if (bytes.length > MAX_FRAME) throw new Error('Extension frame exceeds 1 MiB')
    await new Promise((resolve, reject) => this.socket.write(bytes, error => error ? reject(error) : resolve()))
  }

  #read(chunk) {
    this.#buffer = Buffer.concat([this.#buffer, chunk])
    while (true) {
      const end = this.#buffer.indexOf(10)
      if (end < 0) break
      if (end + 1 > MAX_FRAME) return this.close(new Error('Extension frame exceeds 1 MiB'))
      const frame = this.#buffer.subarray(0, end)
      this.#buffer = this.#buffer.subarray(end + 1)
      let message
      try { message = JSON.parse(frame.toString('utf8')) } catch { return this.close(new Error('Invalid extension JSON')) }
      if (!message || message.jsonrpc !== '2.0') return this.close(new Error('Invalid extension RPC envelope'))
      this.#dispatch(message)
    }
    if (this.#buffer.length >= MAX_FRAME) this.close(new Error('Extension frame exceeds 1 MiB'))
  }

  #dispatch(message) {
    if (!message.method) {
      if (typeof message.id !== 'string' ||
          Object.hasOwn(message, 'result') === Object.hasOwn(message, 'error')) {
        this.close(new RuntimeError('INVALID_RESULT', 'Invalid JSON-RPC response'))
        return
      }
      const pending = this.#pending.get(message.id)
      if (!pending) return
      this.#pending.delete(message.id)
      pending.cleanup()
      if (message.error) {
        const error = new RuntimeError(message.error.data?.code ?? 'HOST_ERROR', message.error.message)
        if (message.error.data?.classified === true) classifiedRemoteErrors.add(error)
        pending.reject(error)
      } else pending.resolve(message.result)
      return
    }
    if (message.method === 'request.cancel') {
      this.#active.get(message.params?.id)?.abort(new Error('Caller cancelled extension operation'))
      return
    }
    if (message.id !== undefined && (typeof message.id !== 'string' || this.#active.has(message.id))) {
      this.close(new RuntimeError('INVALID_ARGUMENT', 'Invalid or duplicate JSON-RPC request id'))
      return
    }
    const controller = new AbortController()
    if (message.id !== undefined) this.#active.set(message.id, controller)
    Promise.resolve().then(async () => {
      try {
        const result = await this.handler(message.method, message.params, controller.signal)
        if (message.id !== undefined) await this.#send({ jsonrpc: '2.0', id: message.id, result: result ?? null })
      } catch (error) {
        if (message.id !== undefined) await this.#send({ jsonrpc: '2.0', id: message.id,
          error: { code: -32000, message: String(error?.message ?? error).slice(0, 2048),
            data: { code: typeof error?.code==='string'?error.code:'EXTENSION_ERROR',
              classified: error instanceof RuntimeError || (error instanceof Error &&
                (classifiedRemoteErrors.has(error) || error.name === 'HostError' && typeof error.code === 'string')),
              ...(typeof error?.name==='string'?{name:error.name}:{}) } } })
      }
    }).catch(() => {}).finally(() => this.#active.delete(message.id))
  }

  close(reason = new Error('Host connection closed')) {
    if (this.#closed) return
    this.#closed = true
    for (const pending of this.#pending.values()) { pending.cleanup(); pending.reject(reason) }
    this.#pending.clear()
    for (const active of this.#active.values()) active.abort(reason)
    this.#active.clear()
    this.socket.destroy()
  }
}
