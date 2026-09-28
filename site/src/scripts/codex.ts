import { EGGS, foundCount, onFound, registerEgg, runEgg } from './easter-eggs';

/**
 * The codex: one card per easter egg, progress persisted in localStorage.
 * Undiscovered eggs keep their name but hide how to fire them, so the panel
 * teases without spoiling. The footer badge stays hidden until the first
 * discovery, which keeps a first-time visitor's footer clean.
 */
export function initCodex(): void {
	const panel = document.getElementById('codex');
	const list = panel?.querySelector<HTMLElement>('[data-codex-list]');
	const counter = panel?.querySelector<HTMLElement>('[data-codex-count]');
	const badge = document.querySelector<HTMLElement>('[data-egg-badge]');
	const toast = document.querySelector<HTMLElement>('[data-toast]');
	if (!panel || !list || !counter) return;

	let open = false;
	let toastTimer = 0;
	let lastKey = 0;

	function render(found: ReadonlySet<string>): void {
		const { found: n, total } = foundCount();
		counter.textContent = `${n}/${total} signals caught`;

		list.replaceChildren(
			...EGGS.map((egg) => {
				const got = found.has(egg.id);
				const li = document.createElement('li');
				li.className = got ? 'codex-item got' : 'codex-item';

				const dot = document.createElement('span');
				dot.className = 'codex-dot';
				dot.setAttribute('aria-hidden', 'true');

				const body = document.createElement('div');
				body.className = 'codex-body';

				const name = document.createElement('h3');
				name.className = 'codex-name';
				name.textContent = egg.name;

				const hint = document.createElement('code');
				hint.className = 'codex-hint';
				hint.textContent = got ? egg.hint : '? ? ?';

				const note = document.createElement('p');
				note.className = 'codex-note';
				note.textContent = got ? egg.note : 'unidentified signal';

				body.append(name, hint, note);
				li.append(dot, body);
				return li;
			})
		);

		if (badge) {
			badge.hidden = n === 0;
			badge.textContent = `◆ ${n}/${total}`;
		}
	}

	function setOpen(next: boolean): void {
		open = next;
		if (next) {
			panel.hidden = false;
			// Force a layout so the drawer has a start position to slide from.
			void panel.offsetWidth;
			document.body.classList.add('codex-open');
		} else {
			document.body.classList.remove('codex-open');
			window.setTimeout(() => {
				if (!open) panel.hidden = true;
			}, 380);
		}
		panel.setAttribute('aria-hidden', String(!open));
	}

	function showToast(id: string): void {
		const egg = EGGS.find((e) => e.id === id);
		if (!egg || !toast) return;
		const { found, total } = foundCount();
		toast.replaceChildren();
		const star = document.createElement('span');
		star.className = 'toast-star';
		star.textContent = '◆';
		const label = document.createElement('span');
		label.className = 'toast-label';
		label.textContent = `new signal · ${egg.name}`;
		const count = document.createElement('span');
		count.className = 'toast-count';
		count.textContent = `${found}/${total}`;
		toast.append(star, label, count);
		toast.classList.add('show');
		clearTimeout(toastTimer);
		toastTimer = window.setTimeout(() => toast.classList.remove('show'), 3600);
	}

	document.addEventListener('orbitron:egg-found', (e) => {
		showToast((e as CustomEvent<{ id: string }>).detail.id);
	});

	document.addEventListener('keydown', (e) => {
		if (e.key === 'Escape' && open) {
			setOpen(false);
			return;
		}
		if (e.metaKey || e.ctrlKey || e.altKey || e.key.length !== 1) return;
		// Shift only: a bare `c` would fight every typed word that contains one.
		if (!e.shiftKey || e.key.toLowerCase() !== 'c') return;
		const el = e.target as HTMLElement | null;
		if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable)) return;
		const now = Date.now();
		if (now - lastKey < 600) return;
		lastKey = now;
		runEgg('codex');
	});

	panel.querySelector('[data-codex-close]')?.addEventListener('click', () => setOpen(false));

	badge?.addEventListener('click', () => {
		// Opening the codex from the footer is itself the discovery.
		if (badge.hidden) return;
		runEgg('codex');
	});

	registerEgg('codex', () => setOpen(!open));

	onFound(render);
	setOpen(false);
}
