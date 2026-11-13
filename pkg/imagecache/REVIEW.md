# Review Notes

- A blob keeps one temporary across all of its download attempts, so a later
  attempt resumes from it. Any step that can fail after the temporary is
  closed must leave it in a state the next attempt acts on without writing
  (verified: retry the rename only). A closed file handed back to a retry
  fails every remaining attempt with "file already closed", hiding the error
  that mattered.
