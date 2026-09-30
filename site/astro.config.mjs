// @ts-check
import { readFileSync } from 'node:fs';
import { defineConfig } from 'astro/config';

// Advertised release version, read from the VERSION file in the repository root
// so the site never carries its own hardcoded copy. "dev" mirrors the default in
// internal/build, which is what an unstamped local build reports too.
let version = 'dev';
try {
	version = readFileSync(new URL('../VERSION', import.meta.url), 'utf8').trim();
} catch {
	// No VERSION file (e.g. site/ copied out on its own): keep the fallback.
}

// https://astro.build/config
export default defineConfig({
	site: 'https://getorbitron.app',
	vite: {
		// Textual replacement, so the name must not collide with anything in the
		// client bundle. Declared for TypeScript in src/env.d.ts.
		define: {
			__ORBITRON_VERSION__: JSON.stringify(version),
		},
		build: {
			// Keep component <script>s as external files so the strict
			// Content-Security-Policy (script-src 'self') keeps them working.
			assetsInlineLimit: 0,
		},
	},
});
