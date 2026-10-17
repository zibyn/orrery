package nodeproto

import (
	"encoding/json"
	"testing"
)

func TestCompatible(t *testing.T) {
	tests := []struct {
		name                   string
		cpMajor, cpMinor       uint32
		agentMajor, agentMinor uint32
		accepted, outdated     bool
	}{
		{"same version", 1, 3, 1, 3, true, false},
		{"older minor is accepted and flagged outdated", 1, 3, 1, 2, true, true},
		{"newer minor is refused", 1, 3, 1, 4, false, false},
		{"different major is refused", 1, 3, 2, 3, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accepted, outdated := compatible(tt.cpMajor, tt.cpMinor, tt.agentMajor, tt.agentMinor)
			if accepted != tt.accepted || outdated != tt.outdated {
				t.Fatalf("compatible = (%v, %v), want (%v, %v)", accepted, outdated, tt.accepted, tt.outdated)
			}
		})
	}
}

func TestCoreCapabilitiesEnforced(t *testing.T) {
	snapshot := func(entries map[string]string) json.RawMessage {
		var list []CapabilityEntry
		for _, name := range Required {
			value, ok := entries[name]
			if !ok {
				continue
			}
			list = append(list, CapabilityEntry{Name: name, Value: value})
		}
		raw, err := json.Marshal(CapabilityReport{Runtime: "docker", Entries: list})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	all := map[string]string{}
	for _, name := range Required {
		all[name] = ValueEnforced
	}

	if !CoreEnforced(snapshot(all)) {
		t.Fatal("all required capabilities enforced should be schedulable")
	}
	for _, name := range Required {
		entries := map[string]string{}
		for k, v := range all {
			entries[k] = v
		}
		entries[name] = ValueUnsupported
		if CoreEnforced(snapshot(entries)) {
			t.Fatalf("%s unsupported must not be schedulable", name)
		}
	}
	if CoreEnforced(nil) || CoreEnforced(json.RawMessage("null")) || CoreEnforced(json.RawMessage("not json")) {
		t.Fatal("missing or malformed capabilities must not be schedulable")
	}
}
