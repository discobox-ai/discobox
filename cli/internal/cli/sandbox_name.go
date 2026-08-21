package cli

import (
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// sandboxNameIsTitle reports whether the display name the server computed for a
// sandbox came from its primary terminal's window title rather than the name
// the sandbox is configured with. Rename edits the configured name, so a row
// showing a title is a row a rename would not visibly change.
//
// A sandbox with no configured name falls back to its ID, which rename does
// change what is shown for — so only a differing name counts as a title.
func sandboxNameIsTitle(sb apimodel.Sandbox) bool {
	return sb.Config.Name != "" && sb.DisplayName != sb.Config.Name
}
