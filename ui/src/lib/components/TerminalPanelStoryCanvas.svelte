<script lang="ts">
	import TerminalPanel, { type TerminalHandle } from './TerminalPanel.svelte';
	import type { Sandbox } from './types';

	let {
		activeSandbox,
		initialData,
		onData,
		onResize,
		streamInitialData = false
	}: {
		activeSandbox: Sandbox;
		initialData?: string;
		onData?: (data: string) => void;
		onResize?: (size: { rows: number; cols: number }) => void;
		streamInitialData?: boolean;
	} = $props();

	let terminalHandle = $state<TerminalHandle | undefined>();
	let streamVersion = 0;

	function splitTerminalChunks(data: string) {
		return data.match(/[^\r\n]*(?:\r\n|\n|\r)?/g)?.filter((chunk) => chunk.length > 0) ?? [data];
	}

	function handleData(data: string) {
		onData?.(data);

		window.setTimeout(() => {
			if (data === '\r') {
				terminalHandle?.write('\r\nmock-websocket$ ');
				return;
			}
			if (data === '\x03') {
				terminalHandle?.write('^C\r\nmock-websocket$ ');
				return;
			}
			if (data === '\x7f') {
				terminalHandle?.write('\b \b');
				return;
			}
			terminalHandle?.write(data);
		}, 20);
	}

	$effect(() => {
		if (!streamInitialData || !terminalHandle || initialData === undefined) {
			return;
		}

		const version = ++streamVersion;
		const chunks = splitTerminalChunks(initialData);
		let index = 0;
		let timeout: ReturnType<typeof setTimeout> | undefined;

		function writeNextChunk() {
			if (version !== streamVersion || !terminalHandle) {
				return;
			}

			const chunk = chunks[index];
			if (chunk !== undefined) {
				terminalHandle.write(chunk);
				index += 1;
			}

			if (index < chunks.length) {
				timeout = setTimeout(writeNextChunk, 8);
			}
		}

		timeout = setTimeout(writeNextChunk, 20);

		return () => {
			streamVersion += 1;
			if (timeout) {
				clearTimeout(timeout);
			}
		};
	});
</script>

<TerminalPanel
	{activeSandbox}
	connectionStatus="connected"
	bind:handle={terminalHandle}
	initialData={streamInitialData ? undefined : initialData}
	onData={handleData}
	{onResize}
/>
