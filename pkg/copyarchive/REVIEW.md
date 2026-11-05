# Copy review notes

- Pin the selected root after resolving its caller-selected ancestors; reject
  descendant destination symlinks with rooted operations. Rejecting every
  symlink from the volume root breaks macOS /tmp and /var and symlinked homes.
- A copy-mode content write must enforce or explicitly refuse ordinary write
  preconditions before any mutation. Never silently ignore ifExists, ifMissing
  or ifVersion when switching publication implementations.
