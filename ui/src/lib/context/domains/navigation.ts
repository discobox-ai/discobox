import type { Context } from '$lib/context/context.types';

export function setDesktopSidebarOpen(context: Context, open: boolean): void {
	context.view.navigation.desktopSidebarOpen = open;
}

export function toggleDesktopSidebarOpen(context: Context): void {
	setDesktopSidebarOpen(context, !context.view.navigation.desktopSidebarOpen);
}
