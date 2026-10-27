package cli

import (
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// A pool's host credential says which key the host holds, how far it reaches
// and when it expires, and the driver's machine token beside it.
func TestFormatPoolHostCredential(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		credential apimodel.PoolHostCredential
		want       string
	}{
		{
			apimodel.PoolHostCredential{
				Key: apiclientgen.NewOptPoolHostCredentialKey(apiclientgen.PoolHostCredentialKeyIssued), KeyId: apiclientgen.NewOptString("key-1"),
				Scope: apiclientgen.NewOptString("org"), ExpiresAt: apiclientgen.NewOptDateTime(now.Add(72 * time.Hour)), MachineToken: apiclientgen.NewOptBool(true),
			},
			"an issued key key-1, expiring 3 days from now, reaching the org; and the driver's machine token, which reaches the org",
		},
		{
			apimodel.PoolHostCredential{Key: apiclientgen.NewOptPoolHostCredentialKey(apiclientgen.PoolHostCredentialKeyProvider)},
			"the provider's own key, which discobox never expires, renews or revokes",
		},
		{
			apimodel.PoolHostCredential{MachineToken: apiclientgen.NewOptBool(true)},
			"the driver's machine token, which reaches the org",
		},
	} {
		if got := formatPoolHostCredential(tc.credential, now); got != tc.want {
			t.Errorf("formatPoolHostCredential() = %q, want %q", got, tc.want)
		}
	}
}
