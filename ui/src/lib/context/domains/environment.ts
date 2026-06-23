import type { Context } from '$lib/context/context.types';
import { getAppEnvironment, type WindowControlsMode } from '$lib/environment';

export function hydrateEnvironment(context: Context): void {
	const environment = getAppEnvironment();

	context.data.environment.isDesktop = environment.kind === 'desktop';
	context.data.environment.platform = environment.platform;
	context.view.app.environment.isMacPlatform = environment.platform === 'macos';
	context.view.app.environment.windowControls = environment.windowControls;
}

export function setWindowControls(context: Context, windowControls: WindowControlsMode): void {
	context.view.app.environment.windowControls = windowControls;
}

export async function toggleWindowMaximized(context: Context): Promise<void> {
	if (!context.data.environment.isDesktop) {
		return;
	}

	await getAppEnvironment().toggleWindowMaximized();
}
