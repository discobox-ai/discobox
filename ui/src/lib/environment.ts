export type AppPlatform = 'macos' | 'windows' | 'linux' | 'web' | 'unknown';
export type WindowControlsMode = 'macos' | 'windows' | 'linux' | 'none' | 'macos-fullscreen';

export type DesktopEnvironmentBridge = {
	kind: 'electron';
	platform: AppPlatform;
	windowControls: WindowControlsMode;
	getWindowControls: () => Promise<WindowControlsMode>;
	onWindowControlsChange: (callback: (windowControls: WindowControlsMode) => void) => () => void;
	windowMinimize: () => Promise<void>;
	windowMaximize: () => Promise<void>;
	windowUnmaximize: () => Promise<void>;
	windowIsMaximized: () => Promise<boolean>;
	windowClose: () => Promise<void>;
	openExternalUrl: (url: string) => Promise<void>;
};

export type AppEnvironment = {
	kind: 'desktop' | 'web';
	platform: AppPlatform;
	windowControls: WindowControlsMode;
	getWindowControls: () => Promise<WindowControlsMode>;
	onWindowControlsChange: (callback: (windowControls: WindowControlsMode) => void) => () => void;
	toggleWindowMaximized: () => Promise<void>;
	openExternalUrl: (url: string) => Promise<void>;
};

const webEnvironment: AppEnvironment = {
	kind: 'web',
	platform: 'web',
	windowControls: 'none',
	getWindowControls: async () => 'none',
	onWindowControlsChange: () => () => {},
	toggleWindowMaximized: async () => {},
	openExternalUrl: async (url) => {
		window.open(url, '_blank', 'noopener,noreferrer');
	}
};

function desktopEnvironment(bridge: DesktopEnvironmentBridge): AppEnvironment {
	return {
		kind: 'desktop',
		platform: bridge.platform,
		windowControls: bridge.windowControls,
		getWindowControls: bridge.getWindowControls,
		onWindowControlsChange: bridge.onWindowControlsChange,
		toggleWindowMaximized: async () => {
			if (await bridge.windowIsMaximized()) {
				await bridge.windowUnmaximize();
				return;
			}

			await bridge.windowMaximize();
		},
		openExternalUrl: bridge.openExternalUrl
	};
}

export function getAppEnvironment(): AppEnvironment {
	if (typeof window === 'undefined') {
		return webEnvironment;
	}

	const bridge = window.__DISCOBOX_DESKTOP__;
	if (!bridge) {
		return webEnvironment;
	}

	return desktopEnvironment(bridge);
}
