package cli

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// egressFlags are one level's egress policy as flags (ADR 26-10-01-032): the
// project, a pool, a harness config, or one discobox. A level is sent whole,
// so an update that names only some of its parts keeps the others as stored.
type egressFlags struct {
	allow         []string
	denyByDefault bool
	passthrough   []string
}

// egressHelp explains the levels once, for every command that sets one.
const egressHelp = `Egress is decided per discobox from four levels: its project, its pool, its
harness, and its own. A discobox is restricted when any level sets
--egress-deny-by-default, and then reaches only the union of every level's
--egress-allow entries and the hosts its harness image declares; an allow list
alone restricts nothing. An entry is a host ("github.com" is that host alone),
a wildcard ("*.github.com", "registry.*", "*"), an IP, or a CIDR, never a port.
--egress-allow replaces this level's list; on an update, --clear-egress-allow
empties it.

--egress-passthrough names hosts whose HTTPS proxy CONNECT is spliced to the
origin unread instead of intercepted, so TLS stays end to end (ADR
26-10-31-819): for a client that pins its certificate, or a protocol the proxy
does not read. It never admits a host, and nothing inside such a tunnel is read:
no credential is swapped into it, and only the connection, its TLS hello, and
its byte counts are audited. A discobox a person creates passes through every
level's hosts; one a discobox created, only its own level's, which a discobox
may set only to hosts its own passthrough covers. On an update,
--clear-egress-passthrough empties this level's list.`

func addEgressFlags(cmd *cobra.Command, f *egressFlags, level string) {
	cmd.Flags().StringSliceVar(&f.allow, "egress-allow", nil, "Destinations "+level+" adds to a restricted discobox's allow list (repeatable or comma-separated)")
	cmd.Flags().BoolVar(&f.denyByDefault, "egress-deny-by-default", false, "Restrict "+level+"'s discoboxes to the allow lists of their levels; --egress-deny-by-default=false lifts this level's restriction")
	cmd.Flags().StringSliceVar(&f.passthrough, "egress-passthrough", nil, "Hosts whose HTTPS CONNECT "+level+" passes through unread instead of intercepting (repeatable or comma-separated)")
}

// addEgressUpdateFlags is addEgressFlags on an update, which can also empty
// the level's lists.
func addEgressUpdateFlags(cmd *cobra.Command, f *egressFlags, level string) {
	addEgressFlags(cmd, f, level)
	addClearFlag(cmd, "egress-allow", "Empty "+level+"'s allow list")
	addClearFlag(cmd, "egress-passthrough", "Empty "+level+"'s passthrough list")
}

func (f *egressFlags) changed(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("egress-allow") || cmd.Flags().Changed("egress-deny-by-default") || cmd.Flags().Changed("egress-passthrough")
}

// entries is a list flag as given. An empty entry is refused rather than
// dropped: emptying a list is its --clear- flag, and an entry that says
// nothing is more likely a variable that expanded to nothing.
func entries(cmd *cobra.Command, name string, given []string) ([]string, error) {
	if !cmd.Flags().Changed(name) {
		return nil, nil
	}
	empty := errors.New("--" + name + ": an entry is empty; --clear-" + name + " empties the list")
	out := make([]string, 0, len(given))
	for _, entry := range given {
		if entry = strings.TrimSpace(entry); entry == "" {
			return nil, empty
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil, empty
	}
	return out, nil
}

// create is the level a new discobox is given, and whether the flags named one.
func (f *egressFlags) create(cmd *cobra.Command) (apimodel.EgressPolicy, bool, error) {
	if !f.changed(cmd) {
		return apimodel.EgressPolicy{}, false, nil
	}
	allow, err := entries(cmd, "egress-allow", f.allow)
	if err != nil {
		return apimodel.EgressPolicy{}, false, err
	}
	passthrough, err := entries(cmd, "egress-passthrough", f.passthrough)
	if err != nil {
		return apimodel.EgressPolicy{}, false, err
	}
	return egressPolicy(allow, f.denyByDefault, passthrough, cmd.Flags().Changed("egress-passthrough")), true, nil
}

// update is the level to send, and whether the flags named one. current reads
// the level as stored, and is called only when a part was left out and has to
// be kept.
func (f *egressFlags) update(cmd *cobra.Command, current func() (apimodel.EgressPolicy, error)) (apimodel.EgressPolicy, bool, error) {
	allowChanged := cmd.Flags().Changed("egress-allow") || listCleared(cmd.Flags(), "egress-allow")
	denyChanged := cmd.Flags().Changed("egress-deny-by-default")
	passthroughChanged := cmd.Flags().Changed("egress-passthrough") || listCleared(cmd.Flags(), "egress-passthrough")
	if !allowChanged && !denyChanged && !passthroughChanged {
		return apimodel.EgressPolicy{}, false, nil
	}
	allow, err := entries(cmd, "egress-allow", f.allow)
	if err != nil {
		return apimodel.EgressPolicy{}, false, err
	}
	passthrough, err := entries(cmd, "egress-passthrough", f.passthrough)
	if err != nil {
		return apimodel.EgressPolicy{}, false, err
	}
	deny := f.denyByDefault
	sendPassthrough := passthroughChanged
	if !allowChanged || !denyChanged || !passthroughChanged {
		stored, err := current()
		if err != nil {
			return apimodel.EgressPolicy{}, false, err
		}
		if !allowChanged {
			allow, _ = stored.Allow.Get()
		}
		if !denyChanged {
			deny = stored.DenyByDefault.Or(false)
		}
		if !passthroughChanged {
			passthrough, _ = stored.Passthrough.Get()
			sendPassthrough = len(passthrough) > 0
		}
	}
	return egressPolicy(allow, deny, passthrough, sendPassthrough), true, nil
}

// egressPolicy is the level as sent. Passthrough is sent only when it was
// given, cleared, or is stored (sendPassthrough): a server that predates it
// refuses a level with a field it does not know (ADR 26-10-31-819 §6), and an
// update that touches only the allow list must still reach one.
func egressPolicy(allow []string, deny bool, passthrough []string, sendPassthrough bool) apimodel.EgressPolicy {
	level := apimodel.EgressPolicy{
		Allow:         apiclientgen.NewOptNilStringArray(trimmedEntries(allow)),
		DenyByDefault: apiclientgen.NewOptBool(deny),
	}
	if sendPassthrough {
		level.Passthrough = apiclientgen.NewOptNilStringArray(trimmedEntries(passthrough))
	}
	return level
}

func trimmedEntries(list []string) []string {
	out := []string{}
	for _, entry := range list {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}
