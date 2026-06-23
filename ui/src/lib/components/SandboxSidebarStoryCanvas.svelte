<script lang="ts">
	import SandboxSidebar, {
		type SandboxSidebarGrouping,
		type SandboxSidebarSort,
		type SandboxSidebarTaskState
	} from './SandboxSidebar.svelte';
	import type { Sandbox } from './types';

	let {
		activeSandbox,
		sandboxes,
		homeDirectory,
		initialSort,
		initialGrouping,
		initialVisibleStatuses
	}: {
		activeSandbox: Sandbox;
		sandboxes: Sandbox[];
		homeDirectory?: string;
		initialSort: SandboxSidebarSort;
		initialGrouping: SandboxSidebarGrouping;
		initialVisibleStatuses: SandboxSidebarTaskState[];
	} = $props();
</script>

{#key `${initialSort}:${initialGrouping}:${initialVisibleStatuses.join(',')}`}
	<SandboxSidebar
		{activeSandbox}
		{sandboxes}
		{homeDirectory}
		{initialSort}
		{initialGrouping}
		{initialVisibleStatuses}
	/>
{/key}
