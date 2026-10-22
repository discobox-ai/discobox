# Assets Review Notes

- **Gate every source that can reach content a project could not pull
  anonymously on the pin being the release's.** The watcher's layouts and the
  server's registry keychain hold what this host built or pulled for anyone; a
  project naming such an image in a harness config would otherwise have its
  pool download it through the asset API.
- **Scope is the digests something pinned, never a tag's current answer.** A
  tag that moves — every watcher rebuild in development — would otherwise cut
  off a pool part way through the fetch of the digest it was handed, and a
  name route answering a tag no pin records serves a manifest whose blobs are
  then refused.
- **Anything read from the store that fails its digest is removed, manifests
  included.** A damaged manifest left in place is reported present, never
  fetched again, and fails every read of its pin until the server restarts.
- **A source's memo that it already put a blob in the store never outlives the
  blob: check the store, not the memo.** Prune and damaged-blob removal both
  delete blobs; a source that remembers "imported" past that refuses the pin
  for good, and on
  a development server, where a bundle is only in the watcher's layout, every
  read of it is a 502 until restart.
- **A body is checked against its digest before the bytes that complete it
  go out, not at EOF.** A file reports EOF on the read after its last byte, so
  a check made there runs once a response of the blob's `Content-Length` is
  already whole on the wire: the client reads a complete body that is not the
  blob. `imagecache`'s verifying reader withholds the completing read instead.
