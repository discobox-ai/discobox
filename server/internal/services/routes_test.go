package services

import (
	"reflect"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// A route survives the API both ways, its WebSocket places and their
// conditions included, as the model holds it.
func TestSecretRoutesSurviveTheAPI(t *testing.T) {
	for _, text := range []string{
		"GET api.github.com/repos/*/*/pulls/**",
		"POST app.boxd.sh/api/v1/auth/token body=/api_key",
		`GET *.discord.gg,gateway.discord.gg ws=[{"at":"/d/token","when":{"op":2}},{"at":"/d/token","when":{"op":6,"s":null}},{"at":"/auth"}]`,
		`gateway.discord.gg ws=[{"at":"/d/token","when":{"op":"x"}}]=blocked`,
	} {
		route, err := secretroute.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		served := SecretRoutesFromModel([]model.SecretRoute{route})
		data, err := served[0].MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var input serverapi.SecretRouteInput
		if err := input.UnmarshalJSON(data); err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		back := SecretRoutesToModel([]serverapi.SecretRouteInput{input})
		normalized, err := secretroute.Normalize(back[0])
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(normalized, route) {
			t.Errorf("%s came back as %s", text, secretroute.Format(normalized))
		}
	}
}
