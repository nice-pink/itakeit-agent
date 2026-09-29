import { hydrate, mount } from 'svelte'
import './app.css'
import App from './App.svelte'

const target = document.getElementById('app')!

// Production pages are prerendered (scripts/prerender.js) and hydrate in
// place. In dev the div only holds the static fallback, so clear and mount.
const app = target.hasAttribute('data-prerendered')
  ? hydrate(App, { target })
  : ((target.innerHTML = ''), mount(App, { target }))

export default app
