import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { dirname, join, posix } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const dist = join(root, 'dist');

const fail = (msg) => {
	console.error(`FAIL: ${msg}`);
	process.exit(1);
};

const exists = (p) => existsSync(join(dist, p));

const PAGES = [
	'index.html',
	'404.html',
	'docs/index.html',
	'docs/install/index.html',
	'docs/ansible/index.html',
	'docs/configuration/index.html',
	'docs/mirroring/index.html',
	'docs/api/index.html',
];

const FILES = [
	'_headers',
	'robots.txt',
	'sitemap.xml',
	'site.webmanifest',
	'og.png',
	'apple-touch-icon.png',
	'icon-192.png',
	'icon-512.png',
	'favicon.svg',
	'favicon.ico',
];

let checks = 0;
const ok = (label) => {
	checks += 1;
	console.log(`  ok  ${label}`);
};

// 1. Required pages & static files exist.
for (const page of PAGES) {
	if (!exists(page)) fail(`missing built page: ${page}`);
	ok(`page ${page}`);
}
for (const file of FILES) {
	if (!exists(file)) fail(`missing static file: ${file}`);
	ok(`file ${file}`);
}

// 2. Sitemap has exactly the 7 URLs.
const sitemap = readFileSync(join(dist, 'sitemap.xml'), 'utf8');
const locs = [...sitemap.matchAll(/<loc>([^<]+)<\/loc>/g)].map((m) => m[1]);
if (locs.length !== 7) fail(`sitemap has ${locs.length} URLs, expected 7`);
for (const loc of ['', 'docs/', 'docs/install/', 'docs/ansible/', 'docs/configuration/', 'docs/mirroring/', 'docs/api/']) {
	if (!locs.includes(`https://getorbitron.app/${loc}`)) fail(`sitemap missing https://getorbitron.app/${loc}`);
}
ok('sitemap 7 urls');

// 3. JSON-LD: present + valid on index, absent on 404; 404 is noindex.
const indexHtml = readFileSync(join(dist, 'index.html'), 'utf8');
const notFoundHtml = readFileSync(join(dist, '404.html'), 'utf8');

const ldMatch = indexHtml.match(/<script type="application\/ld\+json">(.*?)<\/script>/s);
if (!ldMatch) fail('index.html missing application/ld+json block');
const graph = JSON.parse(ldMatch[1])?.['@graph'] ?? [];
if (!graph.some((n) => n['@type'] === 'SoftwareApplication')) fail('JSON-LD missing SoftwareApplication');
if (!graph.some((n) => n['@type'] === 'WebSite')) fail('JSON-LD missing WebSite');
ok('index JSON-LD (SoftwareApplication + WebSite)');

if (/application\/ld\+json/.test(notFoundHtml)) fail('404.html must not carry JSON-LD');
if (!/noindex/.test(notFoundHtml)) fail('404.html missing noindex');
ok('404 no JSON-LD + noindex');

// 4. CSP safety: component scripts must be external files, not inlined.
if (/<script type="module">/.test(indexHtml)) fail('inline <script type="module"> present (CSP script-src self would block it)');
if (!/<script type="module" src="\/_astro\//.test(indexHtml)) fail('no external component script referenced from index.html');
ok('scripts external (CSP-safe)');

const headers = readFileSync(join(dist, '_headers'), 'utf8');
for (const needle of ['Content-Security-Policy', 'Strict-Transport-Security', 'Permissions-Policy', 'frame-ancestors']) {
	if (!headers.includes(needle)) fail(`_headers missing ${needle}`);
}
ok('_headers security directives');

// 5. Internal link/asset integrity across every built HTML page.
const htmlFiles = [];
const walk = (dir) => {
	for (const entry of readdirSync(join(dist, dir), { withFileTypes: true })) {
		const rel = posix.join(dir, entry.name);
		if (entry.isDirectory()) walk(rel);
		else if (entry.name.endsWith('.html')) htmlFiles.push(rel);
	}
};
walk('.');

const resolveTarget = (href) => {
	let h = href.trim();
	if (!h || h === '#' || /^(mailto:|tel:|data:|javascript:)/.test(h)) return null;
	if (/^(http:\/\/|https:\/\/|\/\/)/.test(h)) {
		if (!h.startsWith('https://getorbitron.app/')) return null; // external
		h = h.slice('https://getorbitron.app'.length);
	}
	const qidx = h.indexOf('?');
	if (qidx !== -1) h = h.slice(0, qidx);
	const hidx = h.indexOf('#');
	if (hidx !== -1) h = h.slice(0, hidx);
	if (!h) return null;
	// absolute path (/assets/x.png, /docs)
	const rel = h.replace(/^\/+/, '');
	return rel || null;
};

let refs = 0;
const broken = [];
for (const file of htmlFiles) {
	const html = readFileSync(join(dist, file), 'utf8');
	for (const [, value] of html.matchAll(/(?:href|src)="([^"]+)"/g)) {
		refs += 1;
		const rel = resolveTarget(value);
		if (rel === null) continue;
		if (exists(rel)) continue;
		if (exists(`${rel}/index.html`)) continue;
		if (rel.endsWith('/index.html') && exists(rel.slice(0, -'index.html'.length) + 'index.html')) continue;
		broken.push(`${file} -> ${value}`);
	}
}
if (broken.length) fail(`broken internal references (${broken.length}):\n  ${broken.slice(0, 10).join('\n  ')}`);
ok(`internal references (${refs} checked)`);

console.log(`smoke: PASS (${checks} checks, ${refs} internal references)`);