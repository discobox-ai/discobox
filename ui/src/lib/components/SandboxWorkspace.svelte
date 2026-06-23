<script lang="ts">
	import discobotLogo from '$lib/assets/branding/discobot-logo-purple.svg';
	import { Badge } from '$lib/components/ui/badge/index.js';
	import * as Card from '$lib/components/ui/card/index.js';
	import EditorPanel from './EditorPanel.svelte';
	import TerminalPanel from './TerminalPanel.svelte';
	import type { Sandbox, WorkspaceFile } from './types';

	let {
		activeSandbox,
		files,
		codeLines,
		terminalLines
	}: {
		activeSandbox: Sandbox;
		files: WorkspaceFile[];
		codeLines: string[];
		terminalLines: string[];
	} = $props();

	const terminalInitialData = $derived(`${terminalLines.join('\r\n')}\r\n$ `);
</script>

<main class="flex h-full min-h-0 min-w-0 flex-1 flex-col overflow-hidden p-2 md:p-3">
	<Card.Root class="mb-2 shrink-0 shadow-sm">
		<Card.Content
			class="flex flex-col gap-3 px-3 py-3 lg:flex-row lg:items-center lg:justify-between"
		>
			<div class="flex min-w-0 items-center gap-3">
				<img src={discobotLogo} alt="Discobot logo" class="h-7 w-7" />
				<div class="min-w-0">
					<div class="flex items-center gap-2">
						<h1 class="truncate text-sm font-semibold">{activeSandbox.name}</h1>
						<Badge class="h-4 px-1.5 text-[10px]">{activeSandbox.id}</Badge>
					</div>
					<p class="truncate text-xs text-muted-foreground">
						{activeSandbox.directory} · {activeSandbox.branch}
					</p>
				</div>
			</div>
			<div class="grid overflow-hidden rounded-lg border border-border bg-muted/50 sm:grid-cols-3">
				<div class="border-b border-border px-3 py-2 sm:border-r sm:border-b-0">
					<div class="text-[11px] text-muted-foreground">Generation</div>
					<div class="text-sm font-semibold">17</div>
				</div>
				<div class="border-b border-border px-3 py-2 sm:border-r sm:border-b-0">
					<div class="text-[11px] text-muted-foreground">State</div>
					<div class="text-sm font-semibold text-emerald-500">Healthy</div>
				</div>
				<div class="hidden px-3 py-2 sm:block">
					<div class="text-[11px] text-muted-foreground">Provider</div>
					<div class="text-sm font-semibold">{activeSandbox.provider}</div>
				</div>
			</div>
		</Card.Content>
	</Card.Root>

	<div
		class="grid min-h-0 flex-1 grid-rows-[minmax(0,1fr)_240px] gap-2 xl:grid-rows-[minmax(0,1fr)_260px]"
	>
		<EditorPanel {files} {codeLines} />
		<TerminalPanel {activeSandbox} connectionStatus="connected" initialData={terminalInitialData} />
	</div>
</main>
