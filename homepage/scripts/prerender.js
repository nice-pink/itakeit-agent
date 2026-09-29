// Replaces each built page's static fallback with the fully rendered app and
// adds its JSON-LD. Runs after `vite build` and `vite build --ssr`.
import { readFileSync, writeFileSync, rmSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const root = fileURLToPath(new URL('..', import.meta.url))
const ssrDir = resolve(root, 'dist-ssr')
const { pages } = await import(pathToFileURL(resolve(ssrDir, 'entry-server.js')).href)

const APP_DIV = /<div id="app">[\s\S]*?<!--\/fallback-->\s*<\/div>/

for (const page of pages) {
  const path = resolve(root, 'dist', page.file)
  const html = readFileSync(path, 'utf8')
  if (!APP_DIV.test(html)) throw new Error(`${page.file}: no <div id="app"> fallback to replace`)
  const { head, body } = page.render()
  const out = html
    .replace(APP_DIV, () => `<div id="app" data-prerendered>${body}</div>`)
    .replace('</head>', () => `${head}<script type="application/ld+json">${page.jsonLd}</script>\n  </head>`)
  writeFileSync(path, out)
  console.log(`prerendered ${page.file} (${(out.length / 1024).toFixed(0)} KB)`)
}

rmSync(ssrDir, { recursive: true, force: true })
