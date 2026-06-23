<script lang="ts">
	import AppHeader from './AppHeader.svelte';
	import type { WindowControlsMode } from '$lib/environment';

	let {
		sidebarCollapsed,
		windowControls,
		onSidebarToggle,
		onSettingsOpen,
		onTitleBarDoubleClick
	}: {
		sidebarCollapsed: boolean;
		windowControls: WindowControlsMode;
		onSidebarToggle: () => void;
		onSettingsOpen: () => void;
		onTitleBarDoubleClick: () => void;
	} = $props();
</script>

<AppHeader
	{sidebarCollapsed}
	{windowControls}
	{onSidebarToggle}
	{onSettingsOpen}
	{onTitleBarDoubleClick}
/>
