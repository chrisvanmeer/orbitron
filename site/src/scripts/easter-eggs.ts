/**
 * Easter egg registry for the site.
 *
 * Every egg has exactly one entry in EGGS, which doubles as the single source
 * of truth for the codex panel and the `eggs` command in the ORBITRON SHELL.
 * `hint` is only revealed once the egg has been discovered, so the catalog can
 * ship in the bundle without spoiling anything.
 */

export interface Egg {
	id: string;
	name: string;
	hint: string;
	note: string;
}

export const EGGS: Egg[] = [
	{ id: 'star', name: 'Shooting star', hint: 'type "star"', note: 'A single meteor cuts across the sky.' },
	{ id: 'stars', name: 'Meteor shower', hint: 'type "stars"', note: 'A staggered burst of a dozen or so.' },
	{ id: 'eclipse', name: 'Total eclipse', hint: 'type "eclipse"', note: 'The moon swallows the sun, Baily’s beads and all.' },
	{ id: 'sputnik', name: 'Satellite pass', hint: 'type "sputnik"', note: 'Earth rises with a train of satellites on its limb.' },
	{ id: 'warp', name: 'Orbital warp', hint: 'type "orbitron"', note: 'The hero rings light up and complete one full orbit.' },
	{ id: 'shell', name: 'Console', hint: 'press `', note: 'A shell that pretends to be the daemon itself.' },
	{ id: 'codex', name: 'Codex', hint: 'press shift+c', note: 'This panel. Every signal you have caught so far.' },
	{ id: 'konami', name: 'Hyperspace', hint: '↑ ↑ ↓ ↓ ← → ← →', note: 'The starfield stretches out to hyperspeed. The b a at the end is optional nostalgia.' },
	{ id: 'idle', name: 'Deep space', hint: 'wait 15 seconds', note: 'Stop touching anything and the sky starts to drift.' },
	{ id: 'observatory', name: 'Observatory mode', hint: 'press shift+n', note: 'Red night vision for the dark hours. Stays on.' },
	{ id: 'hyperjump', name: 'Hyperjump', hint: 'click the wordmark 3 times', note: 'No destination required.' }
];

const STORE = 'orbitron.eggs.v1';

const runners = new Map<string, () => void>();
const charHandlers: ((ch: string, now: number) => void)[] = [];
const listeners: ((found: ReadonlySet<string>) => void)[] = [];

function readStore(): string[] {
	try {
		const raw = window.localStorage.getItem(STORE);
		if (!raw) return [];
		const parsed: unknown = JSON.parse(raw);
		if (!Array.isArray(parsed)) return [];
		return parsed.filter((v): v is string => typeof v === 'string');
	} catch {
		return [];
	}
}

function writeStore(ids: string[]): void {
	try {
		window.localStorage.setItem(STORE, JSON.stringify(ids));
	} catch {
		/* private mode or blocked storage: discoveries stay in-memory */
	}
}

let found = new Set(readStore());

/** True once the egg has been triggered in this browser. */
export function isFound(id: string): boolean {
	return found.has(id);
}

/** Number of discovered eggs, and the total that exist. */
export function foundCount(): { found: number; total: number } {
	return { found: found.size, total: EGGS.length };
}

/** Subscribe to discovery changes; fires immediately with the current state. */
export function onFound(fn: (found: ReadonlySet<string>) => void): void {
	listeners.push(fn);
	fn(found);
}

/** Mark an egg as discovered (idempotent) and notify codex + shell. */
export function discover(id: string): void {
	if (found.has(id)) return;
	found = new Set(found).add(id);
	writeStore([...found]);
	document.dispatchEvent(new CustomEvent('orbitron:egg-found', { detail: { id } }));
	for (const fn of listeners) fn(found);
}

/** Wire an egg's effect. Called once per egg, at boot. */
export function registerEgg(id: string, run: () => void): void {
	runners.set(id, run);
}

/** Discover an egg and fire its effect. Unknown ids are ignored. */
export function runEgg(id: string): boolean {
	const run = runners.get(id);
	if (!run) return false;
	discover(id);
	document.dispatchEvent(new CustomEvent('orbitron:egg-run', { detail: { id } }));
	run();
	return true;
}

/** Whether an egg has an effect wired up yet. */
export function hasEgg(id: string): boolean {
	return runners.has(id);
}

/** Register a handler for individual printable keypresses. */
export function onChar(fn: (ch: string, now: number) => void): void {
	charHandlers.push(fn);
}

const COMBO_TIMEOUT = 1200;

function tracker(seq: string[], onComplete: () => void) {
	let pos = 0;
	let last = 0;
	return (ch: string, now: number) => {
		if (now - last > COMBO_TIMEOUT) pos = 0;
		last = now;
		if (ch !== seq[pos]) {
			// Allow the sequence to restart anywhere it still lines up.
			pos = ch === seq[0] ? 1 : 0;
		} else {
			pos++;
		}
		if (pos >= seq.length) {
			pos = 0;
			onComplete();
		}
	};
}

/** Type a word to fire an egg, e.g. typedWord('eclipse', 'eclipse'). */
export function typedWord(word: string, id: string): void {
	onChar(tracker([...word], () => runEgg(id)));
}

const ARROWS = ['ArrowUp', 'ArrowUp', 'ArrowDown', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'ArrowLeft', 'ArrowRight'];
const KONAMI_TIMEOUT = 1400;

/**
 * The eight arrows are the whole trigger, forwards or backwards. The classic
 * b a that usually follows is swallowed on purpose: the jump already happened
 * on the last arrow, and running it twice would restart the effect.
 */
function konami(onFire: () => void): (key: string, now: number) => void {
	const fwd = [...ARROWS];
	const rev = [...ARROWS].reverse();
	let pos = 0;
	let last = 0;

	return (key, now) => {
		if (now - last > KONAMI_TIMEOUT) pos = 0;
		last = now;
		if (key === 'b' || key === 'a') return;

		const seq = key === fwd[pos] ? fwd : key === rev[pos] ? rev : null;
		if (!seq) {
			// Let the sequence restart anywhere it still lines up.
			pos = key === fwd[0] || key === rev[0] ? 1 : 0;
			return;
		}
		pos++;
		if (pos >= ARROWS.length) {
			pos = 0;
			onFire();
		}
	};
}

const fireKonami = konami(() => runEgg('konami'));

document.addEventListener('keydown', (e) => {
	const el = e.target as HTMLElement | null;
	if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable)) return;
	if (e.metaKey || e.ctrlKey || e.altKey) return;

	const key = e.key || String.fromCharCode(e.keyCode);
	if (key.startsWith('Arrow')) {
		fireKonami(key, Date.now());
		return;
	}
	if (key.length !== 1) return;

	const ch = key.toLowerCase();
	const now = Date.now();
	for (const fn of charHandlers) fn(ch, now);
});
