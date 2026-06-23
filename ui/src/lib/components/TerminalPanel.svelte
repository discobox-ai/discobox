<script lang="ts">
	import '@wterm/dom/css';
	import CopyIcon from '@lucide/svelte/icons/copy';
	import RotateCcwIcon from '@lucide/svelte/icons/rotate-ccw';
	import SquareTerminalIcon from '@lucide/svelte/icons/square-terminal';
	import type { WTerm } from '@wterm/dom';
	import { Button } from '$lib/components/ui/button/index.js';
	import type { Sandbox } from './types';

	export type TerminalConnectionStatus = 'disconnected' | 'connecting' | 'connected' | 'error';
	export type TerminalHandle = {
		clear: () => void;
		focus: () => void;
		write: (data: string | Uint8Array) => void;
	};

	type Props = {
		activeSandbox: Sandbox;
		connectionStatus?: TerminalConnectionStatus;
		handle?: TerminalHandle;
		initialData?: string | Uint8Array;
		onData?: (data: string) => void;
		onResize?: (size: { rows: number; cols: number }) => void;
		statusMessage?: string;
	};

	const INITIAL_TERMINAL_COLS = 80;
	const INITIAL_TERMINAL_ROWS = 20;
	const COPY_RESET_MS = 1600;
	const RESIZE_GRACE_MS = 120;

	let {
		activeSandbox,
		connectionStatus = 'connecting',
		// eslint-disable-next-line no-useless-assignment -- Svelte exposes this through bind:handle.
		handle: terminalHandle = $bindable<TerminalHandle | undefined>(),
		initialData,
		onData,
		onResize,
		statusMessage
	}: Props = $props();

	let copied = $state(false);
	let ready = $state(false);
	let terminalSize = $state<{ rows: number; cols: number } | null>(null);
	let loadStatus = $state<'booting' | 'ready' | 'error'>('booting');
	let title = $state('workspace');
	let transcript = $state('');

	let terminal: WTerm | null = null;
	let resizeObserver: ResizeObserver | null = null;
	let resizeAnimationFrame: number | null = null;
	let resizeTimeout: ReturnType<typeof setTimeout> | null = null;
	let copyTimeout: ReturnType<typeof setTimeout> | null = null;
	let lastInitialData: string | Uint8Array | undefined;
	let lastResize: { rows: number; cols: number } | null = null;

	const cwd = $derived(`${activeSandbox.id}@sandbox:${activeSandbox.directory}/ui`);
	const sizeLabel = $derived(terminalSize ? `${terminalSize.cols}x${terminalSize.rows}` : title);
	const displayStatus = $derived(loadStatus === 'error' ? 'error' : connectionStatus);
	const statusLabel = $derived(statusMessage ?? displayStatus);
	const statusClass = $derived(
		displayStatus === 'connected'
			? 'bg-emerald-500'
			: displayStatus === 'error'
				? 'bg-destructive'
				: displayStatus === 'connecting'
					? 'bg-ring'
					: 'bg-muted-foreground'
	);

	function clearCopyTimeout() {
		if (copyTimeout) {
			clearTimeout(copyTimeout);
			copyTimeout = null;
		}
	}

	function clearResizeAnimationFrame() {
		if (resizeAnimationFrame !== null) {
			cancelAnimationFrame(resizeAnimationFrame);
			resizeAnimationFrame = null;
		}
	}

	function clearResizeTimeout() {
		if (resizeTimeout) {
			clearTimeout(resizeTimeout);
			resizeTimeout = null;
		}
	}

	function writeTerminalData(data: string | Uint8Array) {
		if (!terminal) {
			return;
		}

		terminal.write(data);
		transcript += typeof data === 'string' ? data : new TextDecoder().decode(data);
	}

	function handleInput(data: string) {
		onData?.(data);
	}

	function cleanupHost(host: HTMLDivElement) {
		host.classList.remove('wterm', 'cursor-blink', 'focused', 'has-scrollback');
	}

	async function copyTranscript() {
		if (typeof navigator === 'undefined' || !navigator.clipboard) {
			return;
		}

		await navigator.clipboard.writeText(transcript);
		copied = true;
		clearCopyTimeout();
		copyTimeout = setTimeout(() => {
			copied = false;
		}, COPY_RESET_MS);
	}

	function resetTerminal() {
		terminal?.write('\x1b[2J\x1b[H');
		transcript = '';
		terminal?.focus();
	}

	function measureCell(host: HTMLElement) {
		const grid = host.querySelector('.term-grid') ?? host;
		const row = document.createElement('div');
		const probe = document.createElement('span');

		row.className = 'term-row';
		row.style.position = 'absolute';
		row.style.visibility = 'hidden';
		probe.textContent = 'W';
		row.appendChild(probe);
		grid.appendChild(row);

		const charWidth = probe.getBoundingClientRect().width;
		const rowHeight = row.getBoundingClientRect().height;
		row.remove();

		if (charWidth <= 0 || rowHeight <= 0) {
			return null;
		}

		return { charWidth, rowHeight };
	}

	function fitTerminal(term: WTerm, host: HTMLDivElement) {
		const bounds = host.parentElement;
		if (!bounds || bounds.clientWidth <= 0 || bounds.clientHeight <= 0) {
			return;
		}

		const boundsStyle = getComputedStyle(host);
		const insetTop = parseFloat(boundsStyle.top) || 0;
		const insetBottom = parseFloat(boundsStyle.bottom) || 0;
		const hostHeight = Math.max(1, bounds.clientHeight - insetTop - insetBottom);
		host.style.setProperty('height', `${hostHeight}px`, 'important');
		host.style.setProperty('max-height', `${hostHeight}px`, 'important');

		const measured = measureCell(host);
		if (!measured) {
			return;
		}

		const nextCols = Math.max(1, Math.floor(host.clientWidth / measured.charWidth));
		const nextRows = Math.max(1, Math.floor(host.clientHeight / measured.rowHeight));

		if (nextCols !== term.cols || nextRows !== term.rows) {
			term.resize(nextCols, nextRows);
			host.style.setProperty('height', `${hostHeight}px`, 'important');
			host.style.setProperty('max-height', `${hostHeight}px`, 'important');
		}

		if (lastResize?.cols === nextCols && lastResize.rows === nextRows) {
			return;
		}

		lastResize = { rows: nextRows, cols: nextCols };
		terminalSize = lastResize;
		onResize?.(lastResize);
	}

	function scheduleFitTerminal(term: WTerm, host: HTMLDivElement) {
		clearResizeTimeout();
		resizeTimeout = setTimeout(() => {
			resizeTimeout = null;
			scheduleImmediateFitTerminal(term, host);
		}, RESIZE_GRACE_MS);
	}

	function scheduleImmediateFitTerminal(term: WTerm, host: HTMLDivElement) {
		clearResizeAnimationFrame();
		resizeAnimationFrame = requestAnimationFrame(() => {
			resizeAnimationFrame = null;
			fitTerminal(term, host);
		});
	}

	function mountTerminal(host: HTMLDivElement) {
		let cancelled = false;
		loadStatus = 'booting';

		void (async () => {
			try {
				const { WTerm } = await import('@wterm/dom');

				if (cancelled || terminal) {
					return;
				}

				const nextTerminal = new WTerm(host, {
					autoResize: false,
					cols: INITIAL_TERMINAL_COLS,
					cursorBlink: true,
					onData: handleInput,
					onTitle: (nextTitle) => {
						title = nextTitle || 'workspace';
					},
					rows: INITIAL_TERMINAL_ROWS
				});
				await nextTerminal.init();

				if (cancelled) {
					nextTerminal.destroy();
					return;
				}

				terminal = nextTerminal;
				terminalHandle = {
					clear: () => {
						nextTerminal.write('\x1b[2J\x1b[H');
						transcript = '';
					},
					focus: () => {
						nextTerminal.focus();
					},
					write: writeTerminalData
				};
				fitTerminal(nextTerminal, host);
				scheduleImmediateFitTerminal(nextTerminal, host);

				resizeObserver = new ResizeObserver(() => {
					if (!cancelled && terminal === nextTerminal) {
						scheduleFitTerminal(nextTerminal, host);
					}
				});
				resizeObserver.observe(host.parentElement ?? host);

				ready = true;
				loadStatus = 'ready';
				if (initialData !== undefined) {
					lastInitialData = initialData;
					writeTerminalData(initialData);
				}
				nextTerminal.focus();
			} catch {
				loadStatus = 'error';
			}
		})();

		return {
			destroy() {
				cancelled = true;
				ready = false;
				loadStatus = 'booting';
				terminalSize = null;
				terminalHandle = undefined;
				lastInitialData = undefined;
				lastResize = null;
				transcript = '';
				clearCopyTimeout();
				clearResizeTimeout();
				clearResizeAnimationFrame();
				resizeObserver?.disconnect();
				resizeObserver = null;
				terminal?.destroy();
				terminal = null;
				cleanupHost(host);
			}
		};
	}

	$effect(() => {
		if (!ready || !terminal || initialData === undefined || initialData === lastInitialData) {
			return;
		}

		lastInitialData = initialData;
		resetTerminal();
		writeTerminalData(initialData);
	});
</script>

<section
	class="relative flex h-full min-h-0 flex-col overflow-hidden rounded-lg border border-border bg-background shadow-sm"
	aria-label="Terminal panel"
>
	<header
		class="flex h-9 shrink-0 items-center justify-between gap-3 border-b border-border bg-muted/30 px-2.5 text-xs"
	>
		<div class="flex min-w-0 items-center gap-2">
			<div
				class="flex h-6 items-center gap-1.5 rounded-md border border-border bg-card px-2 font-medium"
			>
				<SquareTerminalIcon class="size-3.5 text-muted-foreground" />
				<span class="truncate">Terminal</span>
			</div>
			<div class="hidden min-w-0 items-center gap-2 text-muted-foreground sm:flex">
				<span class={`size-2 rounded-full ${statusClass}`} title={statusLabel}></span>
				<span class="truncate">{cwd}</span>
			</div>
		</div>

		<div class="flex shrink-0 items-center gap-1">
			<span class="hidden text-muted-foreground md:inline">{sizeLabel}</span>
			<Button
				type="button"
				variant="ghost"
				size="icon-xs"
				aria-label="Copy terminal transcript"
				title="Copy terminal transcript"
				onclick={copyTranscript}
			>
				<CopyIcon class="size-3.5" />
			</Button>
			<Button
				type="button"
				variant="ghost"
				size="icon-xs"
				aria-label="Reset terminal"
				title="Reset terminal"
				onclick={resetTerminal}
			>
				<RotateCcwIcon class="size-3.5" />
			</Button>
		</div>
	</header>

	<div class="relative min-h-0 flex-1 overflow-hidden bg-terminal-bg text-terminal-fg">
		{#if loadStatus !== 'ready'}
			<div
				class="absolute inset-0 z-10 flex items-center justify-center bg-terminal-bg/80 text-xs text-terminal-fg"
			>
				{loadStatus === 'error'
					? 'Terminal failed to initialize'
					: (statusMessage ?? 'Connecting...')}
			</div>
		{/if}

		<div
			use:mountTerminal
			class="terminal-host absolute inset-3 cursor-text overflow-hidden outline-none [caret-color:transparent]"
		></div>
	</div>

	{#if copied}
		<div
			class="pointer-events-none absolute right-4 bottom-4 rounded-md border border-border bg-popover px-2 py-1 text-xs text-popover-foreground shadow-sm"
		>
			Copied
		</div>
	{/if}
</section>

<style>
	:global(.terminal-host.wterm) {
		--term-font-family: var(--font-mono);

		background: transparent;
		box-sizing: border-box;
		height: auto !important;
		max-height: none !important;
		padding: 0;
		border-radius: 0;
		box-shadow: none;
		overflow-x: hidden;
		overflow-y: hidden;
		scrollbar-gutter: stable;
		scrollbar-color: color-mix(in oklch, var(--terminal-fg) 45%, transparent) transparent;
		scrollbar-width: thin;
	}

	:global(.terminal-host.wterm.has-scrollback) {
		overflow-y: auto;
	}

	:global(.terminal-host.wterm::-webkit-scrollbar) {
		width: 10px;
	}

	:global(.terminal-host.wterm::-webkit-scrollbar-track) {
		background: transparent;
	}

	:global(.terminal-host.wterm::-webkit-scrollbar-thumb) {
		background: color-mix(in oklch, var(--terminal-fg) 38%, transparent);
		border: 3px solid transparent;
		border-radius: 999px;
		background-clip: padding-box;
	}

	:global(.terminal-host.wterm::-webkit-scrollbar-thumb:hover) {
		background: color-mix(in oklch, var(--terminal-fg) 55%, transparent);
		background-clip: padding-box;
	}

	:global(.terminal-host.wterm .term-grid) {
		min-height: 0;
	}
</style>
