package store

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestConfigSetPathRoundTripRestoresAbsence(t *testing.T) {
	orig := json.RawMessage(`{"profile":"coding"}`)
	before, err := ConfigGetPath(orig, []string{"deny"})
	if err != nil || before.Present {
		t.Fatalf("before = %+v, %v; want absent", before, err)
	}
	applied, err := ConfigSetPath(orig, []string{"deny"}, ConfigValue{Present: true, Value: json.RawMessage(`["exec"]`)})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := ConfigGetPath(applied, []string{"deny"})
	if !after.Present || string(after.Value) != `["exec"]` {
		t.Fatalf("after = %+v", after)
	}
	restored, err := ConfigSetPath(applied, []string{"deny"}, before)
	if err != nil {
		t.Fatal(err)
	}
	if !ConfigValuesEqual(ConfigValue{Present: true, Value: restored}, ConfigValue{Present: true, Value: orig}) {
		t.Fatalf("restored %s, want %s", restored, orig)
	}
}

func TestConfigSetPathNestedCreatesAndPrunes(t *testing.T) {
	out, err := ConfigSetPath(nil, []string{"a", "b"}, ConfigValue{Present: true, Value: json.RawMessage(`0`)})
	if err != nil || string(out) != `{"a":{"b":0}}` {
		t.Fatalf("set nested: %s %v", out, err)
	}
	// Explicit zero is present, not absent.
	v, _ := ConfigGetPath(out, []string{"a", "b"})
	if !v.Present || string(v.Value) != "0" {
		t.Fatalf("explicit zero lost: %+v", v)
	}
	out, err = ConfigSetPath(out, []string{"a", "b"}, ConfigValue{})
	if err != nil || string(out) != `{}` {
		t.Fatalf("prune: %s %v", out, err)
	}
}

func TestConfigSetPathRejectsNonObject(t *testing.T) {
	if _, err := ConfigSetPath(json.RawMessage(`[1]`), []string{"x"}, ConfigValue{}); err == nil {
		t.Fatal("want error for non-object config")
	}
	// A scalar intermediate must not be silently replaced by an object.
	if _, err := ConfigSetPath(json.RawMessage(`{"a":5}`), []string{"a", "b"}, ConfigValue{Present: true, Value: json.RawMessage(`1`)}); err == nil {
		t.Fatal("want error for scalar intermediate")
	}
}

func TestConfigValuesEqualIgnoresKeyOrder(t *testing.T) {
	a := ConfigValue{Present: true, Value: json.RawMessage(`{"x":1,"y":[2]}`)}
	b := ConfigValue{Present: true, Value: json.RawMessage(`{"y":[2], "x":1}`)}
	if !ConfigValuesEqual(a, b) {
		t.Fatal("want equal")
	}
	if ConfigValuesEqual(a, ConfigValue{}) {
		t.Fatal("present vs absent must differ")
	}
}

func TestCheckTransitionFrom(t *testing.T) {
	if err := CheckTransitionFrom("pending", []string{"pending"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckTransitionFrom("applied", []string{"pending"}); !errors.Is(err, ErrSuggestionStateConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}
