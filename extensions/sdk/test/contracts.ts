import { HostError } from '@go-bot/plugin-sdk'
import type { Context, ToolDefinition } from '@go-bot/plugin-sdk'

async function contracts(ctx: Context) {
  const definition: ToolDefinition = {
    name: 'echo', description: 'Echo', inputSchema: { type: 'object' },
    async execute(args, call) {
      call.signal.throwIfAborted()
      const result = await ctx.host.approvals.request(call, { reason: 'Confirm business action' })
      if (result !== 'allowed') throw new HostError('NOT_ALLOWED', 'Action was not approved')
      return { content: [{ type: 'text', text: call.cwd }], data: args }
    },
  }
  const registration = await ctx.host.tools.register(definition)
  await registration.dispose()
  const snapshot = await ctx.host.skills.list()
  snapshot.items[0]?.modelInvocable satisfies boolean | undefined
  await ctx.host.credentials.put('mailbox', { kind: 'grant', payload: { refreshToken: 'example' } })
  await ctx.host.credentials.update('mailbox', async current => current)
  await ctx.host.authorization.register({ key: 'login', title: 'Account', methods: [{ id: 'oauth', label: 'Browser' }], async run(session) {
    const callback = await session.callback({ state: 'generated-by-provider-library' })
    callback.url satisfies string
    const query = await callback.wait()
    query.state satisfies string[] | undefined
    // The real provider library validates its response and exchanges the code
    // before the plugin can commit a credential.
  } })
  const services = await ctx.host.mcp.list()
  services[0]?.state satisfies string | undefined
  ctx.on('host/credential-changed', event => { event.key satisfies string })
  // @ts-expect-error The SDK does not expose arbitrary Go calls.
  ctx.host.call('sessions.append', {})
  // @ts-expect-error Core Session management remains in Go.
  ctx.host.sessions.create({})
  // @ts-expect-error There is no global plugin guard.
  ctx.host.tools.registerGuard({})
  // @ts-expect-error Assets are internal Go capabilities.
  ctx.host.assets.image(new Uint8Array(), {})
  // @ts-expect-error Plugins return text/data, not invented attachment references.
  await ctx.host.tools.register({ ...definition, async execute() { return { content: [{ type: 'image', image: {} }] } } })
}
void contracts
