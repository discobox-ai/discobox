# Store Review Notes

- A read that decides what the caller writes next is made on the store a `Store.Transaction` callback was handed, never on the root store. With `databaseReadDsn` set, the root store reads from a replica that may not have the write a moment ago, and a decision made from it is silently wrong rather than failed — applying the configuration file's gateways after the default project moved once resolved `default` to the project it had just left. Inside a transaction every read is the transaction's.
