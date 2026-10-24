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
// so an update that names only one of the two flags keeps the other part as
// stored.
type egressFlags struct {
	allow         []string
	denyByDefault bool
}

// egressHelp explains the levels once, for every command that sets one.
const egressHelp = `Egress is decided per discobox from four levels: its project, its pool, its
harness, and its own. A discobox is restricted when any level sets
--egress-deny-by-default, and then reaches only the union of every level's
--egress-allow entries and the hosts its harness image declares; an allow list
alone restricts nothing. An entry is a host ("github.com" is that host alone),
a wildcard ("*.github.com", "registry.*", "*"), an IP, or a CIDR, never a port.
--egress-allow replaces this level's list; on an update, --clear-egress-allow
empties it.`

func addEgressFlags(cmd *cobra.Command, f *egressFlags, level string) {
	cmd.Flags().StringSliceVar(&f.allow, "egress-allow", nil, "Destinations "+level+" adds to a restricted discobox's allow list (repeatable or comma-separated)")
	cmd.Flags().BoolVar(&f.denyByDefault, "egress-deny-by-default", false, "Restrict "+level+"'s discoboxes to the allow lists of their levels; --egress-deny-by-default=false lifts this level's restriction")
}

// addEgressUpdateFlags is addEgressFlags on an update, which can also empty
// the level's list.
func addEgressUpdateFlags(cmd *cobra.Command, f *egressFlags, level string) {
	addEgressFlags(cmd, f, level)
	addClearFlag(cmd, "egress-allow", "Empty "+level+"'s allow list")
}

func (f *egressFlags) changed(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("egress-allow") || cmd.Flags().Changed("egress-deny-by-default")
}

// entries is --egress-allow as given. An empty entry is refused rather than
// dropped: emptying a list is --clear-egress-allow, and an entry that says
// nothing is more likely a variable that expanded to nothing.
func (f *egressFlags) entries(cmd *cobra.Command) ([]string, error) {
	if !cmd.Flags().Changed("egress-allow") {
		return nil, nil
	}
	entries := make([]string, 0, len(f.allow))
	for _, entry := range f.allow {
		if entry = strings.TrimSpace(entry); entry == "" {
			return nil, errors.New("--egress-allow: an entry is empty; --clear-egress-allow empties the list")
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, errors.New("--egress-allow: an entry is empty; --clear-egress-allow empties the list")
	}
	return entries, nil
}

// create is the level a new discobox is given, and whether the flags named one.
func (f *egressFlags) create(cmd *cobra.Command) (apimodel.EgressPolicy, bool, error) {
	if !f.changed(cmd) {
		return apimodel.EgressPolicy{}, false, nil
	}
	allow, err := f.entries(cmd)
	if err != nil {
		return apimodel.EgressPolicy{}, false, err
	}
	return egressPolicy(allow, f.denyByDefault), true, nil
}

// update is the level to send, and whether the flags named one. current reads
// the level as stored, and is called only when one of the two flags was left
// out and its part has to be kept.
func (f *egressFlags) update(cmd *cobra.Command, current func() (apimodel.EgressPolicy, error)) (apimodel.EgressPolicy, bool, error) {
	cleared := listCleared(cmd.Flags(), "egress-allow")
	allowChanged := cmd.Flags().Changed("egress-allow") || cleared
	denyChanged := cmd.Flags().Changed("egress-deny-by-default")
	if !allowChanged && !denyChanged {
		return apimodel.EgressPolicy{}, false, nil
	}
	allow, err := f.entries(cmd)
	if err != nil {
		return apimodel.EgressPolicy{}, false, err
	}
	deny := f.denyByDefault
	if !allowChanged || !denyChanged {
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
	}
	return egressPolicy(allow, deny), true, nil
}

func egressPolicy(allow []string, deny bool) apimodel.EgressPolicy {
	entries := []string{}
	for _, entry := range allow {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	return apimodel.EgressPolicy{
		Allow:         apiclientgen.NewOptNilStringArray(entries),
		DenyByDefault: apiclientgen.NewOptBool(deny),
	}
}
