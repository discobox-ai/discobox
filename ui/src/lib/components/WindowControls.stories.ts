import type { Meta, StoryObj } from '@storybook/sveltekit';
import WindowControlsStoryCanvas from './WindowControlsStoryCanvas.svelte';

const meta = {
	title: 'Discobot/WindowControls',
	component: WindowControlsStoryCanvas,
	parameters: {
		layout: 'fullscreen'
	},
	argTypes: {
		style: {
			control: 'select',
			options: ['macos', 'windows', 'linux']
		}
	},
	args: {
		style: 'macos',
		maximized: false
	}
} satisfies Meta<typeof WindowControlsStoryCanvas>;

export default meta;

type Story = StoryObj<typeof meta>;

export const Default: Story = {};
