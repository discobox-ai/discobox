# bindingplan review notes

- Every fate a preview shows comes from this package, never from a switch in
  a reader. The CLI's `secret request get`/`approve` and the console's
  approval cards both render it: a rule written again in one of them agrees
  until the server's changes, and then a person approves on a screen that
  says something other than what is done. That happened once with what a
  replacement does to the bindings a discobox holds, and again with what it
  does to the grants held in a variable (ADR 26-11-03-329 §5); both now live
  here (`Answer`, `Replacing`, `NewRoutes`).
