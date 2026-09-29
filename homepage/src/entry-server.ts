// Server entry for build-time prerendering (scripts/prerender.js). Crawlers
// that do not run JavaScript, most AI answer engines among them, read this HTML.
import { render } from 'svelte/server'
import App from './App.svelte'
import { itakeit, repo, site } from './lib/site'

const author = { '@type': 'Organization', '@id': 'https://nice.pink/#org', name: 'nice-pink', url: 'https://nice.pink/' }

const app = {
  '@type': 'SoftwareApplication',
  name: 'itakeit-agent',
  url: `${site}/`,
  image: `${site}/og-image.png`,
  applicationCategory: 'BusinessApplication',
  applicationSubCategory: 'AI agent',
  operatingSystem: 'Linux (Docker)',
  description:
    'A free, self-hosted Claude agent for an itakeit Slack channel. It claims the tasks its skills cover and works them in the task thread with reactions and replies: answers, proposals from read-only tools, or changes that an approver signs off with a reaction.',
  softwareHelp: { '@type': 'CreativeWork', url: repo },
  isAccessibleForFree: true,
  offers: { '@type': 'Offer', price: '0', priceCurrency: 'EUR' },
  author: { '@id': author['@id'] },
  softwareRequirements: `itakeit (${itakeit}/)`,
}

const ld = (graph: object[]) => JSON.stringify({ '@context': 'https://schema.org', '@graph': graph }).replace(/</g, '\\u003c')

export const pages = [
  { file: 'index.html', render: () => render(App), jsonLd: ld([author, { '@type': 'WebSite', name: 'itakeit-agent', url: `${site}/`, publisher: { '@id': author['@id'] } }, app]) },
]
