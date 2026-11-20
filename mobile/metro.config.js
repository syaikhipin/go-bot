const path = require('node:path')
const { getDefaultConfig } = require('expo/metro-config')
const config = getDefaultConfig(__dirname)
// Share transcript, locale and interaction sources with the existing Web client.
config.watchFolders = [...config.watchFolders, path.resolve(__dirname, '../frontend/src')]
// Shared interaction hooks must use this app's React instance. Do not resolve
// a second Web React (or require Web node_modules) when bundling native code.
config.resolver.extraNodeModules = { ...config.resolver.extraNodeModules, react: path.resolve(__dirname, 'node_modules/react') }
module.exports = config
