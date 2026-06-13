<script lang="ts">
	import discobotLogo from '$lib/assets/branding/discobot-logo-purple.svg';
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
</script>

<main class="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden p-3 pl-2">
	<section class="card card-border mb-2 shrink-0 bg-base-100 shadow-sm">
		<div class="card-body flex-row items-center justify-between px-3 py-2">
			<div class="flex min-w-0 items-center gap-3">
				<img src={discobotLogo} alt="Discobot logo" class="h-7 w-7" />
				<div class="min-w-0">
					<div class="flex items-center gap-2">
						<h1 class="truncate text-sm font-semibold">{activeSandbox.name}</h1>
						<span class="badge badge-primary badge-soft badge-xs">{activeSandbox.id}</span>
					</div>
					<p class="truncate text-xs text-base-content/60">
						{activeSandbox.directory} · {activeSandbox.branch}
					</p>
				</div>
			</div>
			<div class="flex items-center gap-2">
				<span class="badge badge-outline badge-sm">generation 17</span>
				<span class="badge badge-success badge-soft badge-sm">healthy</span>
			</div>
		</div>
	</section>

	<div class="grid min-h-0 flex-1 grid-rows-[minmax(0,1fr)_260px] gap-2">
		<EditorPanel {files} {codeLines} />
		<TerminalPanel {activeSandbox} lines={terminalLines} />
	</div>
</main>
