# Database Design

This package opens application database connections and runs migrations. It also
contains the tenant database resolver used for the initial database-sharding
model.

## Tenant Database Resolution

Tenant ID is the database shard key.

- The global database/schema contains only global tables, currently `tenants`
  and `users`. It must not contain tenant-owned runtime data.
- For Postgres, tenants resolve to tenant-specific schemas in the configured
  database. The resolver derives a safe schema name from the tenant ID and opens
  tenant connections with that schema in the `search_path`.
- For SQLite, each tenant resolves to a separate database file. The resolver
  inserts the tenant ID into the configured SQLite file name. For example,
  `sqlite3:///data/discobox.db` and tenant `tenant-1` resolves to
  `sqlite3:///data/discobox.tenant-1.db`.

The resolver opens global and tenant databases lazily, caches connections, and
can run scoped migrations when each database/schema is first opened.

## Migration Scope

Migrations are intentionally split:

- `MigrateGlobal` creates only global tables.
- `MigrateTenant` creates only tenant-scoped tables and the tenant-local durable
  orchestration tables.

The deprecated `Migrate` helper remains tenant-scoped for compatibility. New
callers should choose `MigrateGlobal` or `MigrateTenant` explicitly. This
prevents accidental writes of tenant data to the global/default schema.

## Tenant Lifecycle Direction

Initially, a new user should receive a new tenant ID and that tenant ID should be
used to resolve the database connection. Later, tenant ownership should move to
an organization-like resource, and users should be invited directly into an
existing tenant.

The model still keeps `tenant_id` on tenant-owned rows even when SQLite uses one
file per tenant. This keeps application logic and Postgres behavior consistent
and leaves room for moving tenants between physical shards later.
