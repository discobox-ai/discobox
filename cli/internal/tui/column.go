package tui

import (
	"fmt"
	"strconv"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// A column is one side of the workspace, as the core has it
// (console.Column): a strip of panes with one visible at a time, in the order
// their sessions sort in. What goes in which is the core's (console.Exec.Side);
// the left column holds the primary, whose ending ends the workspace.
type column = console.Column[*pane]

// tabLabel is one pane's tab: the key it answers to behind the leader, its
// name (Model.paneName), and whether it is over. See tabKey for base.
func tabLabel(c *column, i, base int, name string) string {
	p := c.Tabs[i]
	name = fmt.Sprintf("%s %s", tabKey(c, i, base), name)
	if p.Exited {
		name += " ·done"
	}
	// A service that has printed something since you last looked at it says
	// so here, because here is where you are looking when you are not looking
	// at it.
	if p.Unread {
		name += " •"
	}
	return name
}

// tabKey is the key one pane answers to behind the leader, and the badge its
// tab wears so the two are never in doubt: the number the core gives it
// (console.Column.Number), a bare digit for a terminal or a shell, and behind
// the S chord for a service.
func tabKey(c *column, i, base int) string {
	n, service := c.Number(i, base)
	if service {
		return fmt.Sprintf("%s%d", paneServicesKey, n)
	}
	return strconv.Itoa(n)
}
