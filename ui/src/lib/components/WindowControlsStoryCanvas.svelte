<script lang="ts">
	import WindowControls from './WindowControls.svelte';
	import type { WindowControlsStyle } from './window-controls';

	let {
		style,
		maximized
	}: {
		style: WindowControlsStyle;
		maximized: boolean;
	} = $props();
</script>

<WindowControls {style} {maximized} />
