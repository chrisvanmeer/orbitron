import { EGGS, discover, foundCount, isFound, onFound, hasEgg, registerEgg, runEgg } from './easter-eggs';

type Line = { text: string; cls?: string };

interface Command {
	about: string;
	usage?: string;
	run: (args: string[]) => Line[];
}

const REDUCED =
	typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

const pad = (s: string, n: number) => s.padEnd(n);
const padL = (s: string, n: number) => s.padStart(n);

function clock(ms: number): string {
	const total = Math.floor(ms / 1000);
	const h = Math.floor(total / 3600);
	const m = Math.floor((total % 3600) / 60);
	const s = total % 60;
	return `${padL(String(h), 2)}:${padL(String(m), 2)}:${padL(String(s), 2)}`;
}

function coin(n: number): string {
	const hex = '0123456789abcdef';
	let out = '';
	for (let i = 0; i < n; i++) out += hex[Math.floor(Math.random() * 16)];
	return out;
}

export function initShell(): void {
	const shell = document.getElementById('shell');
	const out = shell?.querySelector<HTMLElement>('[data-shell-out]');
	const form = shell?.querySelector<HTMLFormElement>('[data-shell-form]');
	const input = shell?.querySelector<HTMLInputElement>('[data-shell-in]');
	const counter = shell?.querySelector<HTMLElement>('[data-shell-count]');
	if (!shell || !out || !form || !input) return;

	const started = Date.now();

	// ---- output buffer -----------------------------------------------------
	const queue: Line[] = [];
	let typing: { el: HTMLElement; text: string; i: number } | null = null;
	let pumpRaf = 0;

	function scroll() {
		out.scrollTop = out.scrollHeight;
	}

	function pump() {
		if (!typing) {
			const next = queue.shift();
			if (!next) {
				pumpRaf = 0;
				return;
			}
			const el = document.createElement('div');
			el.className = 'sh-line' + (next.cls ? ' sh-' + next.cls : '');
			out.appendChild(el);
			scroll();
			if (REDUCED) {
				el.textContent = next.text;
			} else {
				typing = { el, text: next.text, i: 0 };
			}
		}
		const cur = typing;
		if (cur) {
			cur.i = Math.min(cur.text.length, cur.i + 3);
			cur.el.textContent = cur.text.slice(0, cur.i);
			scroll();
			if (cur.i >= cur.text.length) typing = null;
		}
		pumpRaf = requestAnimationFrame(pump);
	}

	function emit(lines: Line[]) {
		queue.push(...lines);
		if (!pumpRaf) pumpRaf = requestAnimationFrame(pump);
	}

	function clear() {
		queue.length = 0;
		typing = null;
		out.replaceChildren();
	}

	// ---- open / close ------------------------------------------------------
	let open = false;
	let lastFocus: HTMLElement | null = null;

	function setOpen(next: boolean) {
		open = next;
		if (next) {
			shell.hidden = false;
			// Force a layout so the panel has a start position to slide from.
			void shell.offsetWidth;
			document.body.classList.add('shell-open');
			lastFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
			input.focus({ preventScroll: true });
			if (!out.childElementCount) greet();
			// Finding the console is what catches the egg.
			discover('shell');
		} else {
			document.body.classList.remove('shell-open');
			// Drop focus so the single-key shortcuts keep working after closing.
			input.blur();
			window.setTimeout(() => {
				if (!open) shell.hidden = true;
			}, 440);
			lastFocus?.focus({ preventScroll: true });
		}
		shell.setAttribute('aria-hidden', String(!open));
	}

	function greet() {
		emit([
			{ text: `orbitron v${__ORBITRON_VERSION__} — internal Ansible Galaxy mirror daemon`, cls: 'cyan' },
			{ text: '=======================================================', cls: 'dim' },
			{ text: '' },
			{ text: 'type ', cls: 'dim' },
			{ text: 'help', cls: 'yellow' },
			{ text: ' for commands, ', cls: 'dim' },
			{ text: 'eggs', cls: 'yellow' },
			{ text: ' for the ones nobody documents.', cls: 'dim' },
			{ text: '' }
		]);
	}

	// ---- commands ----------------------------------------------------------
	const eggAliases: Record<string, string> = {
		star: 'star',
		stars: 'stars',
		eclipse: 'eclipse',
		sputnik: 'sputnik',
		warp: 'warp',
		konami: 'konami',
		hyperjump: 'hyperjump',
		codex: 'codex',
		observatory: 'observatory'
	};

	function eggLines(id: string): Line[] {
		const egg = EGGS.find((e) => e.id === id);
		if (!egg || !hasEgg(id)) {
			return [
				{ text: `orbitron: ${id} is not wired up in this build.`, cls: 'red' }
			];
		}
		runEgg(id);
		return [
			{ text: `→ ${egg.name.toLowerCase()}. ${egg.note}`, cls: 'pink' }
		];
	}

	const COMMANDS: Record<string, Command> = {
		help: {
			about: 'this list',
			run: (args) => {
				if (args[0] && COMMANDS[args[0]]) {
					const c = COMMANDS[args[0]];
					return [
						{ text: args[0], cls: 'yellow' },
						{ text: '  ' + c.about, cls: 'dim' },
						...(c.usage ? [{ text: 'usage: ' + c.usage, cls: 'dim' }] : [])
					];
				}
				return [
					{ text: 'commands', cls: 'head' },
					{ text: '  help [cmd]          this list' },
					{ text: '  info                this daemon, as it imagines itself' },
					{ text: '  version             print the release stamp' },
					{ text: '  inventory           storage matrix' },
					{ text: '  metrics             prometheus exposition, six lines of it' },
					{ text: '  prune               what the reaper would consider' },
					{ text: '  token new           mint an admin token (cosmetic)' },
					{ text: '  uptime              how long you have been orbiting' },
					{ text: '  whoami              you are, technically' },
					{ text: '  history             what you typed' },
					{ text: '  eggs                the not-documented commands' },
					{ text: '  clear               wipe the buffer' },
					{ text: '  exit                close the console' },
					{ text: '' },
					{ text: 'panels', cls: 'head' },
					{ text: '  shift+c             the codex, the panel on your right' },
					{ text: '  shift+n             observatory mode, red light for the dark hours' },
					{ text: '  `                   this console, from anywhere' },
					{ text: '' },
					{ text: '↑ ↓ for history, tab completes, esc closes.', cls: 'dim' }
				];
			}
		},
		info: {
			about: 'daemon, build and runtime facts',
			run: () => [
				{ text: `orbitron            v${__ORBITRON_VERSION__} · one static binary · CGO_ENABLED=0` },
				{ text: 'build               linux/amd64, linux/arm64 · ~11.5 MiB' },
				{ text: 'galaxy api          V1 + V3, served natively' },
				{ text: 'git sources         github, gitlab' },
				{ text: 'storage             /var/lib/orbitron/storage' },
				{ text: 'layout              content-addressed, sha-256' },
				{ text: 'workers             concurrent clone + download pool' },
				{ text: 'auth                bearer + basic, optional OIDC/Keycloak' },
				{ text: 'observability       /metrics, grafana dashboard included' },
				{ text: 'air-gap             the entire point' },
				{ text: 'uptime              ' + clock(Date.now() - started), cls: 'cyan' }
			]
		},
		version: {
			about: 'the release stamp',
			run: () => [{ text: `orbitron ${__ORBITRON_VERSION__} (go, cgo disabled, no runtime deps)`, cls: 'cyan' }]
		},
		inventory: {
			about: 'storage matrix',
			run: () => [
				{ text: pad('NAMESPACE', 24) + pad('KIND', 12) + padL('ITEMS', 6) + padL('VERSIONS', 10) + padL('ON DISK', 10) },
				{ text: pad('geerlingguy', 24) + pad('role', 12) + padL('14', 6) + padL('38', 10) + padL('2.1 GiB', 10) },
				{ text: pad('geerlingguy.docker', 24) + pad('role', 12) + padL('9', 6) + padL('21', 10) + padL('1.4 GiB', 10) },
				{ text: pad('geerlingguy.kubernetes', 24) + pad('role', 12) + padL('22', 6) + padL('71', 10) + padL('5.8 GiB', 10) },
				{ text: pad('ansible.posix', 24) + pad('collection', 12) + padL('3', 6) + padL('9', 10) + padL('412 MiB', 10) },
				{ text: pad('community.general', 24) + pad('collection', 12) + padL('1', 6) + padL('4', 10) + padL('96 MiB', 10) },
				{ text: '-'.repeat(65) },
				{ text: pad('total', 24) + pad('', 12) + padL('40', 6) + padL('122', 10) + padL('8.4 GiB', 10), cls: 'cyan' },
				{ text: 'never-accessed versions are never pruned.', cls: 'dim' }
			]
		},
		metrics: {
			about: 'prometheus exposition',
			run: () => [
				{ text: '# HELP orbitron_uptime_seconds Daemon uptime in seconds.' },
				{ text: '# TYPE orbitron_uptime_seconds gauge' },
				{ text: 'orbitron_uptime_seconds ' + Math.floor((Date.now() - started) / 1000) },
				{ text: 'orbitron_cached_items 40' },
				{ text: 'orbitron_cached_versions 122' },
				{ text: 'orbitron_storage_bytes 9019431321' },
				{ text: 'orbitron_sync_jobs_total 47' },
				{ text: 'orbitron_pruned_versions_total 3' }
			]
		},
		prune: {
			about: 'what the reaper would consider',
			usage: 'prune [--dry-run]',
			run: (args) => {
				const dry = args.includes('--dry-run') || !args.includes('--yes');
				return [
					{ text: 'scanning access index … 122 versions across 40 items' },
					{ text: '  - community.general 0.2.0    last served 214d ago      38 MiB' },
					{ text: '  - geerlingguy.nginx 1.4.5     last served 187d ago      11 MiB' },
					{ text: '  - 2 versions pinned by a manifest are skipped' },
					...(dry
						? [
								{ text: 'dry run: 2 versions (49 MiB) would be removed. nothing was.', cls: 'yellow' },
								{ text: 'pass --yes when you mean it.', cls: 'dim' }
							]
						: [{ text: 'removed 2 versions. 49 MiB back on the platter.', cls: 'yellow' }])
				];
			}
		},
		token: {
			about: 'mint an admin token',
			usage: 'token new',
			run: (args) => {
				if (args[0] !== 'new') {
					return [
						{ text: 'usage: token new', cls: 'dim' },
						{ text: '  subcommands: new, list, revoke', cls: 'dim' }
					];
				}
				return [
					{ text: 'minting …' },
					{ text: `  orb_admin_${coin(32)}`, cls: 'yellow' },
					{ text: 'cosmetic. this is a website. do not paste it anywhere.', cls: 'dim' }
				];
			}
		},
		uptime: {
			about: 'how long you have been orbiting',
			run: () => [
				{ text: 'you have been on this page for ' + clock(Date.now() - started) + '.', cls: 'cyan' },
				{ text: 'the daemon has been up for much longer, and is very happy about it.', cls: 'dim' }
			]
		},
		whoami: {
			about: 'you are, technically',
			run: () => [
				{ text: 'visitor — unauthenticated, read-only, extremely welcome.', cls: 'cyan' }
			]
		},
		history: {
			about: 'what you typed',
			run: () => {
				if (!history.length) return [{ text: 'nothing yet.', cls: 'dim' }];
				return history.map((h, i) => ({ text: `${padL(String(i + 1), 4)}  ${h}`, cls: 'dim' }));
			}
		},
		eggs: {
			about: 'the not-documented commands',
			run: () => {
				const { found, total } = foundCount();
				const lines: Line[] = [
					{ text: `${found}/${total} signals caught. trigger: how you fire them.`, cls: 'head' },
					{ text: '' }
				];
				for (const egg of EGGS) {
					const got = isFound(egg.id);
					lines.push({
						text: `  ${got ? '✓' : '·'} ${pad(egg.name, 20)} ${got ? egg.hint : '? ? ?'}`,
						cls: got ? 'yellow' : 'dim'
					});
				}
				lines.push({ text: '' });
				lines.push({
					text: found === total
						? 'all of them. the sky thanks you.'
						: 'the rest are still out there somewhere.',
					cls: found === total ? 'yellow' : 'dim'
				});
				return lines;
			}
		},
		clear: { about: 'wipe the buffer', run: () => { clear(); return []; } },
		exit: {
			about: 'close the console',
			run: () => {
				setTimeout(() => setOpen(false), 220);
				return [{ text: 'there is no outside. closing anyway.', cls: 'dim' }];
			}
		}
	};
	COMMANDS.quit = COMMANDS.exit;

	function suggest(name: string): string | null {
		const names = Object.keys(COMMANDS);
		let best: string | null = null;
		let bestD = 3;
		for (const n of names) {
			const d = distance(name, n);
			if (d < bestD) {
				bestD = d;
				best = n;
			}
		}
		return best;
	}

	// Damerau-Levenshtein, capped: only used for "did you mean" hints.
	function distance(a: string, b: string): number {
		const m = a.length;
		const n = b.length;
		if (Math.abs(m - n) > 2) return 99;
		let prev2: number[] = [];
		let prev: number[] = [];
		let cur: number[] = [];
		for (let j = 0; j <= n; j++) prev[j] = j;
		for (let i = 1; i <= m; i++) {
			cur = [i];
			for (let j = 1; j <= n; j++) {
				const cost = a[i - 1] === b[j - 1] ? 0 : 1;
				let v = Math.min(cur[j - 1] + 1, prev[j] + 1, prev[j - 1] + cost);
				if (i > 1 && j > 1 && a[i - 1] === b[j - 2] && a[i - 2] === b[j - 1]) {
					v = Math.min(v, prev2[j - 2] + 1);
				}
				cur.push(v);
			}
			prev2 = prev;
			prev = cur;
			cur = [];
		}
		return prev[n];
	}

	function jokey(raw: string): Line[] | null {
		if (raw.startsWith('sudo')) {
			return [
				{ text: '[sudo] password for visitor: ******' },
				{ text: 'sudo: a 404. this incident has been reported to the vacuum.', cls: 'red' }
			];
		}
		if (/^rm\s+-rf/.test(raw)) {
			return [
				{
					text: 'refused. the cache is content-addressed — deleting it would only create work for the next sync.',
					cls: 'red'
				}
			];
		}
		if (/^(docker|kubectl|helm|podman)\b/.test(raw)) {
			return [{ text: 'this daemon is one static binary. it would like to be alone.', cls: 'dim' }];
		}
		if (raw.startsWith('ansible-playbook')) {
			return [{ text: 'you already won by not needing it.', cls: 'dim' }];
		}
		if (raw === 'yes') {
			return [...Array.from({ length: 12 }, () => ({ text: 'y', cls: 'dim' })), { text: '(stopped. your terminal is smaller than your patience.)', cls: 'dim' }];
		}
		return null;
	}

	// ---- history & completion ---------------------------------------------
	const history: string[] = [];
	let histIdx = 0;

	function execute(raw: string): void {
		const line = raw.trim();
		if (!line) return;
		emit([{ text: 'orbitron ❯ ' + line, cls: 'head' }]);
		history.push(line);
		histIdx = history.length;

		const [name, ...args] = line.split(/\s+/);
		if (eggAliases[name]) {
			emit(eggLines(eggAliases[name]));
			return;
		}
		const joke = jokey(line);
		if (joke) {
			emit(joke);
			return;
		}
		const cmd = COMMANDS[name];
		if (!cmd) {
			const hint = suggest(name);
			emit([
				{ text: `orbitron: unknown command: ${name}`, cls: 'red' },
				...(hint ? [{ text: `did you mean ${hint}?`, cls: 'dim' }] : []),
				{ text: "type 'help' if you are lost, 'eggs' if you are curious.", cls: 'dim' }
			]);
			return;
		}
		emit(cmd.run(args));
	}

	// ---- input handling ----------------------------------------------------
	input.addEventListener('keydown', (e) => {
		if (e.key === 'ArrowUp') {
			e.preventDefault();
			if (!history.length) return;
			histIdx = Math.max(0, histIdx - 1);
			input.value = history[histIdx] ?? '';
			return;
		}
		if (e.key === 'ArrowDown') {
			e.preventDefault();
			histIdx = Math.min(history.length, histIdx + 1);
			input.value = history[histIdx] ?? '';
			return;
		}
		if (e.key === 'Tab') {
			e.preventDefault();
			const parts = input.value.split(/\s+/);
			const last = parts[parts.length - 1] ?? '';
			const pool = Object.keys(COMMANDS);
			const match = pool.find((c) => c.startsWith(last) && c !== last);
			if (match) {
				parts[parts.length - 1] = match;
				input.value = parts.join(' ') + ' ';
			}
			return;
		}
		if (e.key === 'l' && e.ctrlKey) {
			e.preventDefault();
			clear();
		}
	});

	form.addEventListener('submit', (e) => {
		e.preventDefault();
		const value = input.value;
		input.value = '';
		execute(value);
	});

	// ---- wiring ------------------------------------------------------------
	document.addEventListener('keydown', (e) => {
		if (e.metaKey || e.ctrlKey || e.altKey) return;
		const el = e.target as HTMLElement | null;
		if (el && el !== input && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable)) return;
		if (e.key === '`') {
			e.preventDefault();
			setOpen(!open);
			return;
		}
		if (e.key === 'Escape' && open) {
			setOpen(false);
		}
	});

	document.querySelectorAll('[data-shell-open]').forEach((el) => {
		el.addEventListener('click', () => setOpen(!open));
	});
	shell.querySelector('[data-shell-close]')?.addEventListener('click', () => setOpen(false));
	document.addEventListener('click', (e) => {
		if (!open) return;
		if (shell.contains(e.target as Node)) return;
		if ((e.target as HTMLElement)?.closest('[data-shell-open]')) return;
		setOpen(false);
	});

	onFound(() => {
		if (counter) {
			const { found, total } = foundCount();
			counter.textContent = `${found}/${total} caught`;
		}
	});

	// The console is its own effect: the egg is caught by opening it, which
	// setOpen already handles, so the runner is a no-op.
	registerEgg('shell', () => {});
}
