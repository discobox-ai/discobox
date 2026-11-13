package secretroute

import (
	"net/http"
	"strings"
	"testing"
)

func TestBodyPlacesAllowed(t *testing.T) {
	routes := func(texts ...string) []Route {
		var out []Route
		for _, s := range texts {
			r, err := Parse(s)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	exchange := "POST app.boxd.sh/api/v1/auth/token body=/api_key"
	for _, tc := range []struct {
		name        string
		grant, list []Route
		ok          bool
	}{
		{"any host names no place", routes(exchange), routes("*"), false},
		{"the place on the host", routes(exchange), routes("*", exchange), true},
		{"the place on any host", routes(exchange), routes("* body=/api_key"), true},
		{"the place under a suffix", routes(exchange), routes("POST *.boxd.sh/** body=/api_key"), true},
		{"a narrower host still overlaps", routes("POST *.boxd.sh/api/v1/auth/token body=/api_key"), routes(exchange), true},
		{"another place", routes(exchange), routes("POST app.boxd.sh/api/v1/auth/token body=/token"), false},
		{"another host", routes(exchange), routes("POST other.example body=/api_key"), false},
		{"another method", routes(exchange), routes("PUT app.boxd.sh body=/api_key"), false},
		{"a blocked route allows nothing", routes(exchange), routes("*", "app.boxd.sh body=/api_key=blocked"), false},
		{"every place must be named", routes("POST app.boxd.sh body=/api_key,/secret"), routes("POST app.boxd.sh body=/api_key"), false},
		{"header routes are not this check's", routes("api.github.com"), routes("app.boxd.sh body=/api_key"), true},
		{"a blocked grant route grants nothing", routes("app.boxd.sh body=/api_key=blocked"), routes("*"), true},
		{"an empty list inherits or allows nothing", routes(exchange), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := BodyPlacesAllowed(tc.grant, tc.list)
			if (err == nil) != tc.ok {
				t.Fatalf("BodyPlacesAllowed = %v, want ok %v", err, tc.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "body place") {
				t.Fatalf("reason %q does not name the body place", err)
			}
		})
	}
}

// What BodyPlacesAllowed refuses, Decide refuses: the reported grant, a
// secret routed to `*` with a delegation naming the exchange's body place.
func TestBodyPlacesAllowedAgreesWithDecide(t *testing.T) {
	wide, err := Parse("*")
	if err != nil {
		t.Fatal(err)
	}
	exchange, err := Parse("POST app.boxd.sh/api/v1/auth/token body=/api_key")
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Method: http.MethodPost, Host: "app.boxd.sh", URL: "/api/v1/auth/token", Header: http.Header{"Content-Type": {"application/json"}}, At: []string{"/api_key"}}
	for _, secret := range [][]Route{{wide}, {wide, exchange}} {
		allowed, _ := Decide([][]Route{secret, {exchange}}, req)
		if checked := BodyPlacesAllowed([]Route{exchange}, secret) == nil; checked != allowed {
			t.Fatalf("secret %v: BodyPlacesAllowed ok = %v, Decide allowed = %v", secret, checked, allowed)
		}
	}
}
