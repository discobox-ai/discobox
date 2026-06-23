import type { Preview } from '@storybook/sveltekit';
import '../src/app.css';

function installStorybookLayoutStyles() {
	if (document.getElementById('discobox-storybook-layout')) {
		return;
	}

	const style = document.createElement('style');
	style.id = 'discobox-storybook-layout';
	style.textContent = `
		#storybook-root,
		#storybook-root > div {
			height: 100vh;
			min-height: 0;
		}
	`;
	document.head.appendChild(style);
}

const preview: Preview = {
	globalTypes: {
		colorScheme: {
			description: 'Preview color scheme',
			defaultValue: 'light',
			toolbar: {
				title: 'Theme',
				icon: 'circlehollow',
				items: [
					{ value: 'light', title: 'Light' },
					{ value: 'dark', title: 'Dark' }
				],
				dynamicTitle: true
			}
		}
	},
	decorators: [
		(Story, context) => {
			const isDark = context.globals.colorScheme === 'dark';

			installStorybookLayoutStyles();
			document.documentElement.classList.toggle('dark', isDark);
			document.documentElement.dataset.theme = 'default';

			return Story();
		}
	],
	parameters: {
		controls: {
			matchers: {
				color: /(background|color)$/i,
				date: /Date$/i
			}
		}
	}
};

export default preview;
