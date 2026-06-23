import {
	closeSettingsDialog,
	openSettingsDialog,
	setSettingsDialogOpen
} from '$lib/context/domains/dialogs';
import {
	hydrateEnvironment,
	setWindowControls,
	toggleWindowMaximized
} from '$lib/context/domains/environment';
import { setDesktopSidebarOpen, toggleDesktopSidebarOpen } from '$lib/context/domains/navigation';
import { refreshSystemTheme, setColorScheme, setTheme } from '$lib/context/domains/preferences';
import type { CommandOptions, Commands, Context } from '$lib/context/context.types';

type DomainCommand<Args extends unknown[], Return = void> = (
	context: Context,
	...args: Args
) => Return | Promise<Awaited<Return>>;
type DomainCommandFor<T> = T extends (...args: infer Args) => infer Return
	? DomainCommand<Args, Awaited<Return>>
	: never;
type CommandRegistrationSpec<T> = {
	[Group in keyof T]: {
		[Command in keyof T[Group]]: DomainCommandFor<T[Group][Command]>;
	};
};

type RegisteredCommandGroups<T> = {
	[Group in keyof T]: {
		[Command in keyof T[Group]]: T[Group][Command] extends DomainCommand<infer Args, infer Return>
			? (...args: Args) => Promise<Awaited<Return>>
			: never;
	};
};

export function createCommands(context: Context): Commands {
	return register(context, {
		dialogs: {
			setSettingsDialogOpen,
			openSettingsDialog,
			closeSettingsDialog
		},
		navigation: {
			setDesktopSidebarOpen,
			toggleDesktopSidebarOpen
		},
		preferences: {
			setTheme,
			setColorScheme,
			refreshSystemTheme
		},
		environment: {
			hydrateEnvironment,
			setWindowControls,
			toggleWindowMaximized
		}
	} satisfies CommandRegistrationSpec<Commands>);
}

function register<T extends CommandRegistrationSpec<Commands>>(
	context: Context,
	commands: T
): RegisteredCommandGroups<T> {
	const registered = {} as RegisteredCommandGroups<T>;

	for (const groupName in commands) {
		const group = commands[groupName];
		const registeredGroup = {} as RegisteredCommandGroups<T>[typeof groupName];

		for (const commandName in group) {
			const command = group[commandName] as DomainCommand<unknown[], unknown>;
			registeredGroup[commandName] = registerCommand(
				context,
				command
			) as RegisteredCommandGroups<T>[typeof groupName][typeof commandName];
		}

		registered[groupName] = registeredGroup;
	}

	return registered;
}

function registerCommand<Args extends unknown[], Return>(
	context: Context,
	command: DomainCommand<Args, Return>
): (...args: Args) => Promise<Awaited<Return>> {
	return async (...args: Args): Promise<Awaited<Return>> => {
		return Promise.resolve(command(context, ...withoutCommandOptions(command, args)));
	};
}

function withoutCommandOptions<Args extends unknown[]>(
	command: { length: number },
	args: Args
): Args {
	const expectedArgCount = Math.max(command.length - 1, 0);
	if (args.length > expectedArgCount && isCommandOptions(args[expectedArgCount])) {
		return args.slice(0, expectedArgCount) as Args;
	}
	return args;
}

function isCommandOptions(value: unknown): value is CommandOptions {
	return typeof value === 'object' && value !== null && 'wait' in value;
}
