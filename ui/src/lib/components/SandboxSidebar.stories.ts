import type { Meta, StoryObj } from '@storybook/sveltekit';
import SandboxSidebarStoryCanvas from './SandboxSidebarStoryCanvas.svelte';
import type { Sandbox } from './types';

const sandboxes: Sandbox[] = [
	{
		id: 'sbx-ui-117',
		directory: '/home/discobot/workspace/ui',
		name: 'sandbox-sidebar-redesign',
		branch: 'feature/sidebar-list',
		taskState: 'open',
		sandboxState: 'running',
		agentStatus: 'running_completion',
		updated: '4m ago',
		provider: 'Docker',
		diff: { files: 9, additions: 342, deletions: 88 }
	},
	{
		id: 'sbx-api-042',
		directory: '/home/discobot/workspace',
		name: 'api-reconcile-flow',
		branch: 'feature/reconcile-events',
		taskState: 'open',
		sandboxState: 'running',
		agentStatus: 'newly_idle',
		agentStatusMessage: 'Session is waiting for an answer',
		updated: '16m ago',
		provider: 'Docker',
		diff: { files: 18, additions: 712, deletions: 194 }
	},
	{
		id: 'sbx-agent-009',
		directory: 'https://github.com/obot-platform/discobox.git',
		name: 'discobox-feature-branch',
		branch: 'fix/agent-heartbeats',
		taskState: 'closed',
		sandboxState: 'stopped',
		agentStatus: 'newly_idle',
		agentStatusMessage: 'Stopped recently after running',
		updated: '1h ago',
		provider: 'Firecracker',
		diff: { files: 5, additions: 63, deletions: 207 }
	},
	{
		id: 'sbx-docs-031',
		directory: 'https://github.com/obot-platform/discobox.git',
		name: 'discobox-commit-pin',
		branch: '7f3a9c2',
		taskState: 'merged',
		sandboxState: 'creating',
		agentStatus: 'idle',
		updated: '3h ago',
		provider: 'Docker',
		diff: { files: 6, additions: 152, deletions: 18 }
	},
	{
		id: 'sbx-hooks-204',
		directory: 'https://github.com/obot-platform/discobox.git',
		name: 'discobox-release-tag',
		branch: 'v0.8.3',
		taskState: 'merged',
		sandboxState: 'stopped',
		agentStatus: 'idle',
		updated: '1d ago',
		provider: 'Cloud',
		diff: { files: 11, additions: 274, deletions: 41 }
	},
	{
		id: 'sbx-db-088',
		directory: '/home/discobot/workspace/server',
		name: 'sandbox-store-indexes',
		branch: 'perf/sandbox-store-indexes',
		taskState: 'closed',
		sandboxState: 'error',
		agentStatus: 'idle',
		updated: '2d ago',
		provider: 'Docker',
		diff: { files: 2, additions: 12, deletions: 36 }
	}
];

const meta = {
	title: 'Discobot/SandboxSidebar',
	component: SandboxSidebarStoryCanvas,
	parameters: {
		layout: 'fullscreen'
	},
	argTypes: {
		initialSort: {
			control: 'select',
			options: ['updated', 'name', 'diff', 'state']
		},
		initialGrouping: {
			control: 'select',
			options: ['none', 'source', 'state']
		},
		initialVisibleStatuses: {
			control: 'check',
			options: ['open', 'closed', 'merged']
		}
	},
	args: {
		activeSandbox: sandboxes[0],
		sandboxes,
		homeDirectory: '/home/discobot',
		initialSort: 'updated',
		initialGrouping: 'source',
		initialVisibleStatuses: ['open', 'closed', 'merged']
	}
} satisfies Meta<typeof SandboxSidebarStoryCanvas>;

export default meta;

type Story = StoryObj<typeof meta>;

export const Default: Story = {};
