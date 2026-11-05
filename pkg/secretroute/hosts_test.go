package secretroute

import (
	"reflect"
	"testing"
)

func TestOpenRouteHosts(t *testing.T) {
	routes := []Route{
		{Hosts: []string{"*.github.com", "github.com"}},
		{Hosts: []string{"github.com"}, Paths: []string{"/x"}},
		{Hosts: []string{"gist.github.com"}, Blocked: true},
	}
	if got, want := OpenRouteHosts(routes), []string{"*.github.com", "github.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OpenRouteHosts = %q, want %q", got, want)
	}
}
