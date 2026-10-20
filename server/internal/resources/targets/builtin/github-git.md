# Git over HTTPS on github.com

Git clones, fetches and pushes a GitHub repository over HTTPS with git's
"smart HTTP" protocol. The repository `OWNER/REPO` is at
`https://github.com/OWNER/REPO.git`; the `.git` suffix may be left off, and
`OWNER` and `REPO` are matched case-insensitively. Every request below carries
the credential in an `Authorization` header (Basic, with a token as the
password, or Bearer).

## Requests

| Request | What it does |
| --- | --- |
| `GET /OWNER/REPO.git/info/refs?service=git-upload-pack` | Lists the repository's refs before a clone or fetch. Reads. |
| `POST /OWNER/REPO.git/git-upload-pack` | Negotiates and downloads the objects of a clone or fetch. Reads, though it is a POST. |
| `GET /OWNER/REPO.git/info/refs?service=git-receive-pack` | Lists the repository's refs before a push. Reads; it is the first step of every push, and changes nothing. |
| `POST /OWNER/REPO.git/git-receive-pack` | Pushes: updates, creates or deletes refs, and uploads the objects they need. |
| `POST /OWNER/REPO.git/info/lfs/objects/batch` | Git LFS: asks where to download (`"operation": "download"`) or upload (`"operation": "upload"`) large files. |

With protocol version 2 (`Git-Protocol: version=2`), a fetch sends its
commands in the body of `git-upload-pack`: `command=ls-refs` lists refs and
`command=fetch` downloads objects. Both read.

## What a push changes

The body of `git-receive-pack` begins with one pkt-line per ref it changes,
each `OLD-OID NEW-OID REFNAME`, the first followed by a NUL and the client's
capabilities, and the list ended by a flush-pkt (`0000`). Push options, when
the `push-options` capability is sent, follow as more pkt-lines and another
flush-pkt. The packfile follows, unless every command deletes.

Each command is one change to one ref, and a push of several refs is several
changes:

- `OLD-OID` all zeros: the ref is **created** at `NEW-OID`.
- `NEW-OID` all zeros: the ref is **deleted**.
- Otherwise the ref is **updated** from `OLD-OID` to `NEW-OID`.

`REFNAME` is the full name: `refs/heads/BRANCH` for a branch,
`refs/tags/TAG` for a tag. A push to `refs/for/...` or another namespace
is not a branch.

Whether an update is a **force push** — `NEW-OID` not descending from
`OLD-OID` — is not shown by the commands. It depends on the history the server
already holds, so a request alone cannot tell a force push from a fast-forward.
GitHub's branch protection, not the request, is what refuses one.

The response is a report of each ref's status (`ok REFNAME` or
`ng REFNAME REASON`); a ref the server refused was not changed.
