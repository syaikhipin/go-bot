export const name = 'example-catalog-tools'
export const inject = ['exampleCatalog', 'host']

export const Config = {
  '~standard': {
    version: 1,
    vendor: 'go-bot-example',
    validate(input) {
      const value = input ?? {}
      if (!value || typeof value !== 'object' || Array.isArray(value) ||
          typeof value.toolName !== 'string' || !/^[a-z][a-z0-9_]{0,63}$/.test(value.toolName) ||
          (value.prefix !== undefined && typeof value.prefix !== 'string')) {
        return { issues: [{ message: 'Provide toolName (letters, digits, underscores) and an optional string prefix' }] }
      }
      return { value: { toolName: value.toolName, prefix: value.prefix ?? '' } }
    },
  },
}

export async function apply(ctx, config) {
  await ctx.host.tools.register({
    name: config.toolName,
    description: 'Look up a key in the example catalog. Returns whether the key exists and its description.',
    concurrency: 'parallel',
    inputSchema: { type: 'object', properties: { key: { type: 'string' } }, required: ['key'], additionalProperties: false },
    async execute(args, execution) {
      execution.signal.throwIfAborted()
      const text = ctx.exampleCatalog.lookup(args.key)
      const message = text === undefined ? 'No matching entry.' : config.prefix + text
      return { content: [{ type: 'text', text: message }], data: { found: text !== undefined, text: message } }
    },
  })
}
