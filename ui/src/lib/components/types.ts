export type Sandbox = {
	id: string;
	directory: string;
	name: string;
	branch: string;
	taskState: 'open' | 'closed' | 'merged';
	sandboxState: 'creating' | 'running' | 'error' | 'stopped';
	agentStatus: 'running_completion' | 'idle' | 'newly_idle';
	agentStatusMessage?: string;
	updated: string;
	provider: string;
	diff: {
		files: number;
		additions: number;
		deletions: number;
	};
};

export type WorkspaceFile = {
	name: string;
	state: string;
};
