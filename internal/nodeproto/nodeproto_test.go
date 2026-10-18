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
		raw, err := json.Marshal(CapabilityReport{RuntimeProvider: "docker", Entries: list})
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

// ADR-0009: Local Runtime reports process/mount namespace unsupported and the
// caller accepts that by requesting runtime: local, so that fact must not make
// the Node unavailable. Every other required capability still applies.
func TestCoreEnforcedByProvider(t *testing.T) {
	report := func(provider string, values map[string]string) json.RawMessage {
		var entries []CapabilityEntry
		for name, value := range values {
			entries = append(entries, CapabilityEntry{Name: name, Value: value})
		}
		raw, err := json.Marshal(CapabilityReport{RuntimeProvider: provider, Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	local := map[string]string{
		CapProcessMountNamespace: ValueUnsupported,
		CapCPUMemoryPIDLimits:    ValueEnforced,
		CapWorkspaceRWMount:      ValueEnforced,
		CapExecSupervisor:        ValueEnforced,
	}
	if !CoreEnforced(report("local", local)) {
		t.Fatal("a Local Node without namespaces must still be schedulable for local requests")
	}
	if CoreEnforced(report("docker", local)) {
		t.Fatal("a Docker Node without process/mount namespaces must not be schedulable")
	}
	local[CapCPUMemoryPIDLimits] = ValueUnsupported
	if CoreEnforced(report("local", local)) {
		t.Fatal("a Local Node without CPU/memory/PID limits must not be schedulable")
	}
}

func TestRuntimeProviderAndCapabilityValue(t *testing.T) {
	raw, err := json.Marshal(CapabilityReport{
		RuntimeProvider: "local",
		Entries:         []CapabilityEntry{{Name: CapGPUDeviceVisibility, Value: ValueBestEffort}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := RuntimeProvider(raw); got != "local" {
		t.Fatalf("RuntimeProvider = %q, want local", got)
	}
	if got := CapabilityValue(raw, CapGPUDeviceVisibility); got != ValueBestEffort {
		t.Fatalf("CapabilityValue = %q, want best_effort", got)
	}
	if got := CapabilityValue(raw, CapResourceMount); got != "" {
		t.Fatalf("CapabilityValue for an absent entry = %q, want empty", got)
	}
	if RuntimeProvider(nil) != "" || CapabilityValue(nil, CapResourceMount) != "" {
		t.Fatal("missing capabilities must have no provider or values")
	}
}
