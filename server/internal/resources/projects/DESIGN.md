# Projects Design

`internal/resources/projects` owns project read service behavior.

- Project listing may use the authenticated user principal from `internal/auth`
  to scope results.
- Project creation/default initialization currently remains in
  `internal/service` because it is application startup policy.

