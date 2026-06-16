import type { Context } from '$lib/context/context.types';

export function setSettingsDialogOpen(context: Context, open: boolean): void {
	context.view.app.dialogs.settings.open = open;
}

export function openSettingsDialog(context: Context): void {
	setSettingsDialogOpen(context, true);
}

export function closeSettingsDialog(context: Context): void {
	setSettingsDialogOpen(context, false);
}
