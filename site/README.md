# Orbitron — website (getorbitron.app)

Marketing landing + quickstart page for [Orbitron](https://github.com/chrisvanmeer/orbitron),
an Ansible Galaxy & Git mirror daemon. Built with [Astro](https://astro.build) (static output).

## Development

```sh
npm install
npm run dev        # local dev server on localhost:4321
npm run build      # static build to ./dist/
npm run preview    # preview the production build locally
```

Start the dev server in background mode when needed: `astro dev --background`.

## Structure

```
astro.config.mjs    site URL + build config
public/
  assets/           logo.svg, architecture.svg, ui.png (copied from repo assets/)
  favicon.svg       orbit favicon (from internal/web/assets)
src/
  layouts/Layout.astro
  components/       Navbar, Hero, Features, Architecture, Dashboard,
                    Quickstart, Ecosystem, CtaSection, Footer, CodeBlock
  styles/global.css cyberpunk neon theme
  pages/index.astro
```

Assets in `public/assets/` are copies of the originals in the repo root
(`assets/logo.svg`, `assets/architecture.svg`, `assets/ui.png`) plus
`internal/web/assets/favicon.svg`. Update them from those sources when they change.

## Deploying to Cloudflare Pages

Connect this repository to Cloudflare Pages and configure:

- **Framework preset:** Astro
- **Root directory:** `site`
- **Build command:** `npm ci && npm run build`
- **Build output directory:** `dist`
- **Custom domain:** `getorbitron.app`