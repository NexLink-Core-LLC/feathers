package environment

import (
	"reflect"
	"testing"
)

// TestExtraPortVars verifies the ordering contract eggs rely on: additional
// allocations come out sorted by IP then port, numbered from 1, with the
// default allocation excluded and invalid ports skipped.
func TestExtraPortVars(t *testing.T) {
	var a Allocations
	a.DefaultMapping.Ip = "0.0.0.0"
	a.DefaultMapping.Port = 7777
	a.Mappings = map[string][]int{
		// Deliberately unsorted, including the default (must be excluded) and
		// an invalid port (must be skipped).
		"0.0.0.0": {7779, 7777, 7778, 0},
	}

	got := a.ExtraPortVars()
	want := []string{
		"SERVER_IP_1=0.0.0.0", "SERVER_PORT_1=7778",
		"SERVER_IP_2=0.0.0.0", "SERVER_PORT_2=7779",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtraPortVars() = %#v, want %#v", got, want)
	}
}

// TestExtraPortVarsMultiIP verifies deterministic ordering across multiple IPs
// and that the default is only excluded on its own IP (the same port number on
// a different IP is a distinct allocation).
func TestExtraPortVarsMultiIP(t *testing.T) {
	var a Allocations
	a.DefaultMapping.Ip = "10.0.0.2"
	a.DefaultMapping.Port = 7777
	a.Mappings = map[string][]int{
		"10.0.0.2": {7777, 7778},
		"10.0.0.1": {7777},
	}

	got := a.ExtraPortVars()
	want := []string{
		"SERVER_IP_1=10.0.0.1", "SERVER_PORT_1=7777",
		"SERVER_IP_2=10.0.0.2", "SERVER_PORT_2=7778",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtraPortVars() = %#v, want %#v", got, want)
	}
}

// TestExtraPortVarsNone: a server with only its default allocation gets no
// extra variables.
func TestExtraPortVarsNone(t *testing.T) {
	var a Allocations
	a.DefaultMapping.Ip = "0.0.0.0"
	a.DefaultMapping.Port = 7777
	a.Mappings = map[string][]int{"0.0.0.0": {7777}}

	if got := a.ExtraPortVars(); len(got) != 0 {
		t.Errorf("ExtraPortVars() = %#v, want empty", got)
	}
}
