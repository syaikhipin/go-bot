export const name = 'example-catalog-provider'

// Standard Schema is a Cordis configuration contract. This example has no
// runtime dependencies; real plugins may use Schemastery, Zod or another
// Standard Schema implementation instead.
export const Config = {
  '~standard': {
    version: 1,
    vendor: 'go-bot-example',
    validate(input) {
      const value = input ?? {}
      if (!value || typeof value !== 'object' || Array.isArray(value) ||
          !value.entries || typeof value.entries !== 'object' || Array.isArray(value.entries) ||
          Object.values(value.entries).some(text => typeof text !== 'string')) {
        return { issues: [{ message: 'entries must be an object of string values', path: ['entries'] }] }
      }
      return { value: { entries: { ...value.entries } } }
    },
  },
}

export function apply(ctx, config) {
  // Cordis withdraws the service when this instance is disabled or removed.
  // Consumers declare the dependency instead of polling for it.
  ctx.provide('exampleCatalog', {
    lookup(key) { return Object.hasOwn(config.entries, key) ? config.entries[key] : undefined },
  })
}
