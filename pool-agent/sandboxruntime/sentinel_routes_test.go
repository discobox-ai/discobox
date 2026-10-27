package sandboxruntime

import (
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	poolagentapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// The route lists the control plane sends with a sentinel reach the proxy as
// lists, in order and each kept apart (ADR 26-10-27-880 §4): merging them
// would turn "every list allows" into "any route allows".
func TestProxySentinelRoutesKeepEachList(t *testing.T) {
	in := map[string][][]workerapimodel.SentinelRoute{
		"SENT": {
			{{Host: "*"}},
			{
				{Host: "api.github.com", Path: poolagentapi.NewOptString("/repos/acme")},
				{Host: "api.github.com", Path: poolagentapi.NewOptString("/repos/acme/admin"), Blocked: poolagentapi.NewOptBool(true)},
			},
		},
	}
	want := map[string][][]secretroute.Route{
		"SENT": {
			{{Host: secretroute.AnyHost}},
			{{Host: "api.github.com", Path: "/repos/acme"}, {Host: "api.github.com", Path: "/repos/acme/admin", Blocked: true}},
		},
	}
	if got := proxySentinelRoutes(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("proxySentinelRoutes = %+v, want %+v", got, want)
	}
}
