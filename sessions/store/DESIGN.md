# Sessions Store Design

The store package owns durable session metadata. The daemon is the only writer
for normal API state; supervisors write small runtime exit files that the daemon
reconciles into this database after restart.
