import type { Meta, StoryObj } from '@storybook/sveltekit';
import { fn } from 'storybook/test';
import AppHeaderStoryCanvas from './AppHeaderStoryCanvas.svelte';

const meta = {
	title: 'Discobot/AppHeader',
	component: AppHeaderStoryCanvas,
	parameters: {
		layout: 'fullscreen'
	},
	argTypes: {
		windowControls: {
			control: 'select',
			options: ['macos', 'windows', 'linux', 'none', 'macos-fullscreen']
		}
	},
	args: {
		sidebarCollapsed: false,
		windowControls: 'macos',
		onSidebarToggle: fn(),
		onSettingsOpen: fn(),
		onTitleBarDoubleClick: fn()
	}
} satisfies Meta<typeof AppHeaderStoryCanvas>;

export default meta;

type Story = StoryObj<typeof meta>;

export const Default: Story = {};
