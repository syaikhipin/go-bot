import { fileURLToPath } from 'node:url'
import { defineConfig, searchForWorkspaceRoot } from 'vite'

export default defineConfig({
  build: {
    license: { fileName: 'assets/licenses.txt' },
    rolldownOptions: {
      output: { postBanner: '/* Bundled dependency licenses at /assets/licenses.txt */' },
    },
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    strictPort: true,
    fs: { allow: [searchForWorkspaceRoot(process.cwd()), fileURLToPath(new URL('../docs/providers.md', import.meta.url))] },
    proxy: {
      // OAuth callbacks return through this same public /v1 route.
      '/v1': { target: 'http://127.0.0.1:8080', changeOrigin: false },
    },
  },
})
