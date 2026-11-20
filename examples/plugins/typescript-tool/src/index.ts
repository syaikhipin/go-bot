import type { Context, JSONValue } from '@go-bot/plugin-sdk'

export const name = 'example-typescript-tool'
export const inject = ['host']

export async function apply(ctx: Context) {
  await ctx.host.tools.register({
    name: 'echo',
    description: 'Return a short note from this example integration.',
    inputSchema: {
      type: 'object', properties: { text: { type: 'string', maxLength: 1000 } },
      required: ['text'], additionalProperties: false,
    },
    async execute(args, call) {
      call.signal.throwIfAborted()
      const text = readNote(args)
      return { content: [{ type: 'text', text }], data: { text } }
    },
  })
}

function readNote(value: JSONValue): string {
  if (!value || typeof value !== 'object' || !('text' in value) || typeof value.text !== 'string') {
    throw new TypeError('A note needs a string text field')
  }
  return value.text
}
