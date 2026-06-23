import type { Meta, StoryObj } from '@storybook/sveltekit';
import { fn } from 'storybook/test';
import TerminalPanelStoryCanvas from './TerminalPanelStoryCanvas.svelte';
import type { Sandbox } from './types';

const activeSandbox: Sandbox = {
	id: 'sbx-ui-117',
	directory: '/home/discobot/workspace',
	name: 'sandbox-shell-mock',
	branch: 'feature/ui-shell',
	taskState: 'open',
	sandboxState: 'running',
	agentStatus: 'running_completion',
	updated: '8m ago',
	provider: 'Docker',
	diff: { files: 8, additions: 281, deletions: 74 }
};

const lines = [
	'$ go tool task dev:server',
	'[discobox] watching Docker worker image inputs',
	'[air] building ./cmd/discobox-server',
	'[server] listening on :8080',
	'$ pnpm --dir ui run dev --host 0.0.0.0 --port 5173 --strictPort',
	'VITE v8.0.16  ready in 412 ms',
	'Local:   http://localhost:5173/',
	'Network: http://172.18.0.2:5173/',
	'$ git status --short',
	' M ui/src/lib/components/TerminalPanel.svelte',
	'?? ui/src/lib/components/TerminalPanel.stories.ts'
];

function terminalStream(lines: string[]) {
	return `${lines.join('\r\n')}\r\n`;
}

const scrollbackLines = Array.from({ length: 80 }, (_, index) => {
	const step = String(index + 1).padStart(2, '0');
	if (index % 8 === 0) {
		return `[test:${step}] running fixture group terminal-scrollback-${step}`;
	}
	if (index % 8 === 3) {
		return `[test:${step}] stdout chunk ${step}: ${'data '.repeat(10).trim()}`;
	}
	if (index % 8 === 6) {
		return `[test:${step}] stderr warning: retryable sandbox event observed`;
	}
	return `[test:${step}] ok terminal output line ${step}`;
});

const blankScrollbackLines = Array.from({ length: 12 }, (_, groupIndex) => [
	`[blank:${String(groupIndex + 1).padStart(2, '0')}] begin intentional blank output`,
	'',
	'',
	'',
	`[blank:${String(groupIndex + 1).padStart(2, '0')}] end intentional blank output`
]).flat();

const initialData = `${terminalStream(lines)}mock-websocket$ `;
const scrollbackData = `${terminalStream([
	...lines,
	'[scrollback] TOP OF GENERATED HISTORY',
	'$ pnpm --dir ui run test:unit -- --run terminal-scrollback',
	...scrollbackLines,
	...blankScrollbackLines,
	'[summary] 80 generated output rows rendered for scrollback verification',
	'[scrollback] BOTTOM OF GENERATED HISTORY'
])}mock-websocket$ `;

const meta = {
	title: 'Discobot/TerminalPanel',
	component: TerminalPanelStoryCanvas,
	parameters: {
		layout: 'fullscreen'
	},
	args: {
		activeSandbox,
		initialData,
		onData: fn(),
		onResize: fn()
	}
} satisfies Meta<typeof TerminalPanelStoryCanvas>;

export default meta;

type Story = StoryObj<typeof meta>;

export const Default: Story = {};

export const TallTranscript: Story = {
	args: {
		initialData: scrollbackData,
		streamInitialData: true
	}
};
