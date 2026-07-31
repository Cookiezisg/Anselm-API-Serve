package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// The Model shape must match OpenAI: id/object/owned_by present, and crucially
// NO "created" field (the gateway invents no per-model timestamp).
func TestModelShapeOmitsCreated(t *testing.T) {
	b, err := json.Marshal(Model{ID: "client-picked-model", Object: ObjectModel, OwnedBy: OwnedBy})
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	if strings.Contains(js, "created") {
		t.Fatalf("model JSON must omit created, got %s", js)
	}
	for _, want := range []string{`"id":"client-picked-model"`, `"object":"model"`, `"owned_by":"anselm-gateway"`} {
		if !strings.Contains(js, want) {
			t.Fatalf("model JSON missing %s, got %s", want, js)
		}
	}
}

// The wire constants are the fixed contract values.
func TestWireConstants(t *testing.T) {
	if ObjectList != "list" || ObjectModel != "model" || OwnedBy != "anselm-gateway" {
		t.Fatalf("constants drifted: list=%q model=%q owner=%q", ObjectList, ObjectModel, OwnedBy)
	}
}

// An empty list must serialize as "data":[] (non-nil), never "data":null.
func TestEmptyEnvelopeMarshalsAsEmptyArray(t *testing.T) {
	b, err := json.Marshal(ListEnvelope{Object: ObjectList, Data: []Model{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.Contains(got, `"data":[]`) {
		t.Fatalf("empty envelope = %s, want data:[]", got)
	}
}
