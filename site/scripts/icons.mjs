import sharp from 'sharp';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const src = join(root, 'scripts', 'icon.svg');
const svg = Buffer.from((await import('node:fs')).readFileSync(src, 'utf8'));

const sizes = {
	'apple-touch-icon.png': 180,
	'icon-192.png': 192,
	'icon-512.png': 512,
};

for (const [file, size] of Object.entries(sizes)) {
	const out = join(root, 'public', file);
	await sharp(svg, { density: 288 })
		.resize(size, size)
		.png({ compressionLevel: 9 })
		.toFile(out);
	console.log(`icon: wrote ${size}x${size} png -> ${out}`);
}