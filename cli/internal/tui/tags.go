package tui

// The tags are the narrowest of the header filter's three (filter.go), after
// the server and the folder: they narrow the list to the discoboxes carrying
// every tag chosen, the way `discobox ls --tag` does when it is repeated. A
// discobox's tags are its own, in the meta file inside it, and the rows show
// the copy the server last heard (ADR 0136). The filter matches a tag as the
// row spells it — `wip`, or `ticket=ENG-12` — so what you pick is what you see
// after the name.
//
// They are only offered once there is a tag to pick: a project nobody tags has
// no use for a choice that can only say "all tags". They are not offered over
// the harnesses and secrets screens, which list no discoboxes.
//
// Their choices and labels are the core's (console.List.FilterChoices,
// console.TagLabel).
