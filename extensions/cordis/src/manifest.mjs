import { readFile, realpath } from 'node:fs/promises'
import { isAbsolute, join, relative, resolve } from 'node:path'
import { RuntimeError } from './errors.mjs'

export const packageName = /^(?:@[a-z0-9._-]+\/)?[a-z0-9][a-z0-9._-]*$/

export async function readManifest(directory, name, { checkCompatibility = true } = {}) {
  if (!packageName.test(name) || name.length > 214) throw new RuntimeError('INVALID_PACKAGE', 'Invalid package name')
  const root = await realpath(join(directory, 'node_modules', name))
  const manifest = JSON.parse(await readFile(join(root, 'package.json'), 'utf8'))
  if (manifest.name !== name) throw new RuntimeError('INVALID_PACKAGE', 'Installed package name does not match')
  if (checkCompatibility && manifest.goBot?.api !== '1') throw new RuntimeError('INCOMPATIBLE_PLUGIN', 'A plugin must declare goBot.api "1"')
  const exports = manifest.exports?.['.'] ?? manifest.exports
  const target = typeof exports === 'string' ? exports : exports?.import ?? exports?.default ?? manifest.main ?? './index.js'
  if (typeof target !== 'string' || isAbsolute(target)) throw new RuntimeError('INVALID_PACKAGE', 'A JavaScript module entry is required')
  const entry = resolve(root, target), local = relative(root, entry)
  if (!local || local.startsWith('../') || isAbsolute(local)) throw new RuntimeError('INVALID_PACKAGE', 'The module entry must be inside its package')
  return { root, manifest, entry }
}
