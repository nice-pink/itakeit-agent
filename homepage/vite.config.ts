import { svelte } from '@sveltejs/vite-plugin-svelte'
import { defineConfig } from 'vite'

// The page imports slack-app-manifest.yaml from the repo root, so dev must be allowed to read it.
export default defineConfig({
  plugins: [svelte()],
  base: './',
  server: { host: '127.0.0.1', fs: { allow: ['..'] } },
})
