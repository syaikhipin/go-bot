package sessionformat

import (
	"encoding/json"
	"testing"
)

func TestNativeSubagentCatalogFoldOwnFactsAndCurrentLocalPayload(t *testing.T) {
	events := []Event{
		{Type: "subagent/catalog", Seq: 0, Data: json.RawMessage(`{"version":0,"childId":"inherited","childCreatedAt":1,"mode":"continuable","label":"Earlier"}`)},
		{Type: "subagent/catalog", Seq: 1, Data: json.RawMessage(`{"version":0,"childId":"local","childCreatedAt":2,"mode":"continuable","label":"Research"}`)},
		{Type: "subagent/catalog", Seq: 2, Data: json.RawMessage(`{"version":1,"childId":"unknown","childCreatedAt":3,"mode":"unknown"}`)},
		{Type: "subagent/catalog", Seq: 3, Data: json.RawMessage(`{"version":2,"childId":"external","childCreatedAt":4,"mode":"external","label":"Review"}`)},
	}
	children, err := FoldSubagentCatalog(events, 1)
	if err != nil || len(children) != 3 || children[0].ChildID != "local" || children[1].Mode != "unknown" || children[2].Mode != "external" {
		t.Fatalf("native child catalog = %+v, %v", children, err)
	}
	for _, raw := range []string{
		`{"version":1,"childId":"x","childCreatedAt":1,"mode":"continuable","label":"X"}`,
		`{"version":1,"childId":"x","childCreatedAt":1,"mode":"unknown"}`,
		`{"version":2,"childId":"x","childCreatedAt":1,"mode":"external","label":"X"}`,
	} {
		if entry, err := DecodeSubagentCatalog(json.RawMessage(raw)); err != nil || entry.ChildID != "x" {
			t.Errorf("rejected upstream subagent/catalog %s: %+v, %v", raw, entry, err)
		}
	}
	for _, raw := range []string{
		`{"version":3,"childId":"x","childCreatedAt":1,"mode":"external"}`,
		`{"version":0,"childId":"x","childCreatedAt":1,"mode":"unknown"}`,
		`{"version":1,"childId":"x","childCreatedAt":1,"mode":"external"}`,
		`{"version":2,"childId":"x","childCreatedAt":1,"mode":"one-shot"}`,
		`{"version":0,"childId":"x","childCreatedAt":1,"mode":"continuable"}`,
		`{"version":0,"childId":"x","childCreatedAt":1,"mode":"external"}`,
		`{"version":0,"childId":"x","childCreatedAt":1,"mode":"continuable","label":"X","extra":true}`,
		`{"version":0,"childId":"x","childCreatedAt":-1,"mode":"continuable","label":"X"}`,
	} {
		if _, err := DecodeSubagentCatalog(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid subagent/catalog %s", raw)
		}
	}
}

func TestNativeSubagentDescriptorV3AndFirstFactAuthority(t *testing.T) {
	raw := json.RawMessage(`{"version":3,"mode":"continuable","provider":"native","label":"Research","agentProvider":"modelstudio","agentModel":"model-x","toolFilter":{"allow":["readfile"],"deny":[]}}`)
	descriptor, err := DecodeSubagentDescriptor(raw)
	if err != nil || descriptor == nil || descriptor.Mode != "continuable" || descriptor.Label == nil || *descriptor.Label != "Research" || descriptor.ToolFilter == nil || len(descriptor.ToolFilter.Allow) != 1 {
		t.Fatalf("native descriptor = %+v, %v", descriptor, err)
	}
	events := []Event{
		{Type: "subagent/descriptor", Seq: 0, Data: raw},
		{Type: "subagent/descriptor", Seq: 1, Data: json.RawMessage(`{"version":3,"mode":"one-shot","provider":"other"}`)},
	}
	folded, err := FoldSubagentDescriptor(events)
	if err != nil || folded == nil || folded.Mode != "continuable" {
		t.Fatalf("first descriptor was not authoritative: %+v, %v", folded, err)
	}
	for _, invalid := range []string{
		`{"version":3,"mode":"continuable","provider":"native"}`,
		`{"version":3,"mode":"continuable","provider":"native","label":"X","extra":1}`,
		`{"version":3,"mode":"one-shot","provider":"native","agentModel":"x"}`,
		`{"version":3,"mode":"continuable","provider":"native","label":"X","toolFilter":{}}`,
	} {
		if _, err := DecodeSubagentDescriptor(json.RawMessage(invalid)); err == nil {
			t.Errorf("accepted invalid v3 descriptor %s", invalid)
		}
	}
	foreign, err := DecodeSubagentDescriptor(json.RawMessage(`{"version":2,"mode":"continuable","provider":"old"}`))
	if err != nil || foreign != nil {
		t.Fatalf("foreign descriptor was classified as current: %+v, %v", foreign, err)
	}
}
