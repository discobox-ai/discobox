package model_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// Every field of an event's data but its time is its state: a change to any
// one of them, a field added later included, is a new event, and the
// backfill brings a latest event that lacks one up to date.
func TestSameStateComparesEveryFieldButAt(t *testing.T) {
	base := model.SandboxEventData{
		ProjectID: "p", SandboxID: "s", Name: "n", At: time.Unix(1, 0),
		DesiredState: "present", State: "ready", RuntimeState: "running", DisplayState: "running",
		Program: &model.SandboxEventProgram{State: "idle", Kind: "", App: "a"},
		Tags:    map[string]string{"k": "v"}, CreatedBySandboxID: "lead",
	}
	other := base
	other.At = time.Unix(2, 0)
	if !base.SameState(other) {
		t.Fatal("the time alone made a difference")
	}
	empty, none := base, base
	empty.Tags, none.Tags = map[string]string{}, nil
	if !empty.SameState(none) {
		t.Fatal("no tags and an empty set of them differ")
	}
	typ := reflect.TypeOf(base)
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Name == "At" {
			continue
		}
		changed := base
		value := reflect.ValueOf(&changed).Elem().Field(i)
		switch value.Kind() {
		case reflect.String:
			value.SetString(value.String() + "-changed")
		case reflect.Map:
			value.Set(reflect.ValueOf(map[string]string{"k": "other"}))
		case reflect.Pointer:
			value.Set(reflect.ValueOf(&model.SandboxEventProgram{State: "working", App: "a"}))
		default:
			t.Fatalf("field %s is a %s this test cannot change: teach it, so SameState is shown to compare it", field.Name, value.Kind())
		}
		if base.SameState(changed) {
			t.Errorf("a change to %s alone is not a change of state", field.Name)
		}
	}
}
