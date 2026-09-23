import sharp from 'sharp';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const src = join(root, 'scripts', 'og.svg');
const out = join(root, 'public', 'og.png');

const svg = Buffer.from((await import('node:fs')).readFileSync(src, 'utf8'));
await sharp(svg, { density: 144 })
	.resize(1200, 630)
	.png({ compressionLevel: 9 })
	.toFile(out);

console.log(`og: wrote 1200x630 png -> ${out}`);