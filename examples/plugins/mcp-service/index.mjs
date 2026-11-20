export const name = 'example-mcp-service'
export const inject = ['host']

export const Config = {
  '~standard': {
    version: 1,
    vendor: 'go-bot-example',
    validate(input) {
      const value = input ?? {}
      const issues = []
      if (!value || typeof value !== 'object' || Array.isArray(value)) return { issues: [{ message: 'Provide an object configuration.' }] }
      if (typeof value.serverName !== 'string' || !/^[A-Za-z0-9_-]{1,32}$/.test(value.serverName)) issues.push({ message: 'Use a serverName of letters, digits, underscores or hyphens.', path: ['serverName'] })
      try {
        const url = new URL(value.url)
        if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) throw new Error('invalid URL')
      } catch { issues.push({ message: 'Provide an HTTP(S) MCP URL without embedded credentials.', path: ['url'] }) }
      if (value.apiKey !== undefined && typeof value.apiKey !== 'string') issues.push({ message: 'apiKey must name a credential reference, not contain a secret.', path: ['apiKey'] })
      if (issues.length) return { issues }
      return { value: { serverName: value.serverName, url: value.url, apiKey: value.apiKey ?? '' } }
    },
  },
}

export async function apply(ctx, config) {
  // Registration uses the built-in Go MCP client. It does not start a second
  // MCP manager in Node. The plugin's lifetime owns this contribution.
  await ctx.host.mcp.register({name: config.serverName, connection: {
    transport: 'streamable-http',
    url: config.url,
    headers: config.apiKey ? { Authorization: 'Bearer ${credential:' + config.apiKey + '}' } : {},
  } })
}
