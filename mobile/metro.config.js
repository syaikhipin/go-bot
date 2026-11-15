const path = require('node:path')
const { getDefaultConfig } = require('expo/metro-config')
const config = getDefaultConfig(__dirname)
// The transcript projector is also used by the existing web client.
config.watchFolders = [...config.watchFolders, path.resolve(__dirname, '../frontend/src')]
module.exports = config
