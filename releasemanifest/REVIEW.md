# Release Manifest Review Notes

- **Anything added to the staged set (`Images.References`) must stage on every
  pool platform.** The cache stages each reference for the platform a pool on
  the host runs and refuses one that does not publish it, so a reference
  published for one platform — a harness bundle is one `linux/amd64` manifest
  — fails the first run of every arm64 host. Staged references feed pool
  daemons; one only the server reads does not belong there.
