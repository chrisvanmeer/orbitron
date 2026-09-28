import { isFound, registerEgg, runEgg } from './easter-eggs';

const OBS = 'orbitron.obs.v1';
const NIGHT_HINT = 'orbitron.night.v1';
const NUDGE = 'orbitron.nudge.v1';

function read(key: string): string | null {
	try {
		return window.localStorage.getItem(key);
	} catch {
		return null;
	}
}

function write(key: string, value: string): void {
	try {
		window.localStorage.setItem(key, value);
	} catch {
		/* storage blocked: observatory mode just won't survive a reload */
	}
}

/**
 * Three small features that are not easter eggs at all, plus one that is:
 * observatory mode (a comfort skin for the dark hours), a one-time night
 * nudge, a one-time console nudge, and the 3x wordmark click that jumps to
 * hyperspace.
 */
export function initModes(): void {
	initObservatory();
	initNightHint();
	initConsoleNudge();
	initHyperjump();
}

function initObservatory(): void {
	const body = document.body;
	const toggles = document.querySelectorAll<HTMLButtonElement>('[data-obs-toggle]');

	function paint() {
		const on = body.classList.contains('obs');
		for (const b of toggles) b.setAttribute('aria-pressed', String(on));
	}

	function toggle() {
		body.classList.toggle('obs');
		write(OBS, body.classList.contains('obs') ? '1' : '0');
		paint();
	}

	// Applied before anything else paints, so a reload never flashes the
	// wrong palette.
	if (read(OBS) === '1') body.classList.add('obs');
	paint();

	registerEgg('observatory', toggle);

	// Clicking the chip is finding the egg, same as the shortcut.
	for (const b of toggles) b.addEventListener('click', () => runEgg('observatory'));

	document.addEventListener('keydown', (e) => {
		if (e.metaKey || e.ctrlKey || e.altKey || e.key.length !== 1) return;
		// Shift only: a bare `n` would fight every typed word that contains one.
		if (!e.shiftKey || e.key.toLowerCase() !== 'n') return;
		const el = e.target as HTMLElement | null;
		if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable)) return;
		runEgg('observatory');
	});
}

function initNightHint(): void {
	if (read(NIGHT_HINT)) return;
	const h = new Date().getHours();
	if (h >= 5 && h < 22) return;

	const el = document.createElement('div');
	el.className = 'night-hint';
	el.setAttribute('role', 'status');

	const text = document.createElement('span');
	text.textContent = "It's night where you are. Red light is easier on the eyes.";

	const go = document.createElement('button');
	go.type = 'button';
	go.className = 'night-go';
	go.textContent = 'observatory mode';
	go.addEventListener('click', () => {
		runEgg('observatory');
		dismiss();
	});

	const close = document.createElement('button');
	close.type = 'button';
	close.className = 'night-x';
	close.setAttribute('aria-label', 'Dismiss');
	close.textContent = '×';
	close.addEventListener('click', dismiss);

	function dismiss() {
		el.classList.add('gone');
		write(NIGHT_HINT, '1');
		setTimeout(() => el.remove(), 400);
	}

	el.append(text, go, close);
	document.body.appendChild(el);
	requestAnimationFrame(() => el.classList.add('in'));
	setTimeout(() => el.classList.add('in'), 1200);
}

function initHyperjump(): void {
	const mark = document.querySelector<HTMLElement>('[data-hyperjump]');
	if (!mark) return;

	const NEEDED = 3;
	const WINDOW = 700;
	let count = 0;
	let timer = 0;
	let last = 0;
	let jumped = false;

	const hint = document.createElement('span');
	hint.className = 'hj-hint';
	hint.setAttribute('aria-hidden', 'true');
	mark.appendChild(hint);

	function paint() {
		mark.style.setProperty('--hj', String(count));
		hint.textContent = '·'.repeat(Math.min(count, NEEDED - 1));
	}

	mark.addEventListener('click', () => {
		const now = Date.now();
		if (now - last > WINDOW) count = 0;
		last = now;
		count++;
		paint();
		clearTimeout(timer);

		if (count >= NEEDED) {
			count = 0;
			paint();
			hint.textContent = jumped ? '↝' : '···';
			runEgg('hyperjump');
			jumped = true;
			setTimeout(paint, 2600);
			return;
		}
		timer = window.setTimeout(() => {
			count = 0;
			paint();
		}, WINDOW);
	});
}

/**
 * One-time nudge after a spell of inactivity: point at the console. Skipped
 * when the console was already found, and when the night hint is on screen, so
 * the two never stack in the same corner.
 */
function initConsoleNudge(): void {
	if (read(NUDGE)) return;
	if (isFound('shell')) return;

	const DELAY = 20_000;
	let interacted = false;
	let timer = 0;

	const wake = () => {
		if (interacted) return;
		interacted = true;
		window.clearTimeout(timer);
		for (const ev of ['keydown', 'pointerdown', 'wheel', 'touchstart'] as const) {
			window.removeEventListener(ev, wake);
		}
	};

	for (const ev of ['keydown', 'pointerdown', 'wheel', 'touchstart'] as const) {
		window.addEventListener(ev, wake, { passive: true });
	}

	timer = window.setTimeout(() => {
		wake();
		if (document.querySelector('.night-hint')) return;

		const el = document.createElement('div');
		el.className = 'console-nudge';
		el.setAttribute('role', 'status');

		const text = document.createElement('span');
		text.textContent = 'this daemon has a console. press ` or tap the ›_ in the bar.';

		const go = document.createElement('button');
		go.type = 'button';
		go.className = 'nudge-go';
		go.textContent = 'open it';
		go.addEventListener('click', () => {
			document.querySelector<HTMLElement>('[data-shell-open]')?.click();
			dismiss();
		});

		const close = document.createElement('button');
		close.type = 'button';
		close.className = 'night-x';
		close.setAttribute('aria-label', 'Dismiss');
		close.textContent = '×';
		close.addEventListener('click', dismiss);

		function dismiss() {
			el.classList.add('gone');
			write(NUDGE, '1');
			setTimeout(() => el.remove(), 400);
		}

		el.append(text, go, close);
		document.body.appendChild(el);
		requestAnimationFrame(() => el.classList.add('in'));
		setTimeout(() => el.classList.add('in'), 1200);
	}, DELAY);
}
