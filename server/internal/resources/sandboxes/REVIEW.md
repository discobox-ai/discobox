# Sandboxes Review Notes

## Values the client hands to host tools

- **A stored path the client hands to a host tool is checked where it is
  stored and where it is used.** A source's paths become the folder an
  editor's `ssh-remote+<host><folder>` URL opens and `{workdir}` in a host
  tool's command, where anything not absolute can name a host instead. The
  client refuses such a folder for every source and record
  (`sandboxSSHFolder`); the server refuses a working directory outside the
  source's directory on create and import (`validateSourceWorkingDirectory`,
  ADR 26-10-15-417). A box can create boxes and an archive is anyone's, so no
  request is trusted to be the client's own.
- **A consumer's check is not the field's validation.** The working directory
  was only ever refused by the pool, because Docker refused it as a
  `WorkingDir`. Removing the consumer removed the check and left a stored value
  unchecked (#234). Before deleting what reads a stored value, find what else
  the deletion stops refusing.
- **Check a client-produced path with `Clean`, never `Rooted`.** `Rooted` is
  the pool's placement reading: it refuses whitespace, so as a validity test it
  refuses every repository whose path has a space in it, which the client
  mirrors as is.
