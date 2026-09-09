package server

import (
	"reflect"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/ports"
)

// sandboxAgentListeningPort is a hand-written projection between two structs
// that are edited for different reasons — one is what the watcher computes, the
// other is what the schema says — so a field added to one and not the other is
// dropped in silence. `declared` was, from the day ADR 0076 added it until
// something finally needed to read it, and `serviceId` would have gone the same
// way.
//
// So this does not list the fields. It fills every exported field of ports.Port
// with a non-zero value by reflection, converts, and requires every field of
// the wire type to have been set — which fails the moment a field is added to
// either side without being mapped.
func TestEveryListeningPortFieldReachesTheWire(t *testing.T) {
	in := ports.Port{}
	value := reflect.ValueOf(&in).Elem()
	for i := range value.NumField() {
		field := value.Field(i)
		if !field.CanSet() {
			t.Fatalf("ports.Port.%s is unexported; this test cannot fill it", value.Type().Field(i).Name)
		}
		fillNonZero(t, value.Type().Field(i).Name, field)
	}

	out := reflect.ValueOf(sandboxAgentListeningPort(in))
	for i := range out.NumField() {
		name := out.Type().Field(i).Name
		if !isSet(out.Field(i)) {
			t.Errorf("SandboxAgentListeningPort.%s was not set from a fully populated ports.Port; "+
				"map it in sandboxAgentListeningPort", name)
		}
	}
}

// fillNonZero puts a distinctive value in a field, so "was it mapped" is
// answerable by looking at the result rather than by trusting the conversion.
func fillNonZero(t *testing.T, name string, field reflect.Value) {
	t.Helper()
	switch field.Kind() {
	case reflect.String:
		field.SetString("filled")
	case reflect.Int, reflect.Int64:
		field.SetInt(6900)
	case reflect.Bool:
		field.SetBool(true)
	case reflect.Slice:
		field.Set(reflect.Append(field, reflect.ValueOf("127.0.0.1")))
	case reflect.Struct:
		if field.Type() == reflect.TypeOf(time.Time{}) {
			field.Set(reflect.ValueOf(time.Unix(1, 0).UTC()))
			return
		}
		t.Fatalf("ports.Port.%s is a struct this test does not know how to fill", name)
	default:
		t.Fatalf("ports.Port.%s is a %s this test does not know how to fill", name, field.Kind())
	}
}

// isSet reports whether a wire field carries anything. ogen's optionals are a
// value plus a Set flag, and an unset one is exactly what a dropped field looks
// like.
func isSet(field reflect.Value) bool {
	if field.Kind() == reflect.Struct {
		if set := field.FieldByName("Set"); set.IsValid() && set.Kind() == reflect.Bool {
			return set.Bool()
		}
	}
	return !field.IsZero()
}

// The other half: an ordinary discovered port carries no empty strings and no
// `"declared": false`, so a client can tell "no service declared this" from
// "a service declared it with an empty name".
func TestAnUndeclaredPortCarriesNoServiceFields(t *testing.T) {
	out := sandboxAgentListeningPort(ports.Port{
		Port:        8080,
		Addresses:   []string{"127.0.0.1"},
		Protocol:    ports.ProtocolHTTP,
		FirstSeenAt: time.Unix(1, 0).UTC(),
	})
	if out.Declared.Set {
		t.Errorf("declared was set on a port nothing declared")
	}
	if out.ServiceId.Set {
		t.Errorf("serviceId was set on a port nothing declared")
	}
	if out.ServiceName.Set {
		t.Errorf("serviceName was set on a port nothing declared")
	}
}

// The desktop, end to end through the projection: the id a client matches on
// and the name it labels the link with both survive.
func TestTheDesktopReachesTheWireWithItsIdentity(t *testing.T) {
	out := sandboxAgentListeningPort(ports.Port{
		Port:        6900,
		Protocol:    ports.ProtocolHTTP,
		Declared:    true,
		ServiceID:   "ai.discobox.desktop",
		ServiceName: "Desktop",
		FirstSeenAt: time.Unix(1, 0).UTC(),
	})
	if !out.Declared.Value {
		t.Errorf("declared did not reach the wire")
	}
	if out.ServiceId.Value != "ai.discobox.desktop" {
		t.Errorf("serviceId = %q, want the declared id", out.ServiceId.Value)
	}
	if out.ServiceName.Value != "Desktop" {
		t.Errorf("serviceName = %q, want the declared name", out.ServiceName.Value)
	}
}
