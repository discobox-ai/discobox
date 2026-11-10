# desktop review notes

- **A link a box or a stranger wrote opens on this machine only when it is
  a web address**, or a `discobox:` link the core answers itself. An issue's
  Markdown is written by anyone, and this machine's URL handler opens a
  `file://` or a custom scheme as readily as a page: `issue.rs`'s
  `open_link` refuses anything but http(s). The move of the app's design out
  of its README once dropped this rule from the docs; a pane's links do not
  hold it yet (#963).
- **A notification only opens what it is about.** Pressing one opens the
  request's card, which still asks for Accept; a press never answers a
  request, and docs that say "answers" misstate it.
