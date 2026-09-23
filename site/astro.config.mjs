// @ts-check
import { defineConfig } from 'astro/config';

// https://astro.build/config
export default defineConfig({
	site: 'https://getorbitron.app',
	vite: {
		build: {
			// Keep component <script>s as external files so the strict
			// Content-Security-Policy (script-src 'self') keeps them working.
			assetsInlineLimit: 0,
		},
	},
});