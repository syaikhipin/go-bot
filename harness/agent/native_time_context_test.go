package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"go-harness/harness/session"
)

func timeContextEvents(t *testing.T, events []sessionformat.Event) []sessionformat.Event {
	t.Helper()
	var found []sessionformat.Event
	for _, event := range events {
		if event.Type != "user/message" {
			continue
		}
		var data struct {
			Source struct {
				Kind     string `json:"kind"`
				Form     string `json:"form"`
				Sections []struct {
					Name string `json:"name"`
					Text string `json:"text"`
				} `json:"sections"`
			} `json:"source"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Source.Kind != "time-context" {
			continue
		}
		if data.Source.Form != "snapshot" || len(data.Source.Sections) != 1 ||
			data.Source.Sections[0].Name != "time-context" || len(data.Content) != 1 ||
			data.Content[0].Type != "text" || data.Content[0].Text != data.Source.Sections[0].Text {
			t.Fatalf("invalid durable time snapshot: %s", event.Data)
		}
		found = append(found, event)
	}
	return found
}

func TestNativeTimeContextIsDurableAndRefreshesByInterval(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "clock", CreatedAt: 1, CWD: "/workspace"}
	journal := &nativeTestJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeInstructionModel{}
	loop, err := NewNativeLoop(live, model, "provider", "model", "system", nil, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.EnableTimeContext("UTC", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "remind me later"); err != nil {
		t.Fatal(err)
	}
	first := timeContextEvents(t, live.DurableEvents())
	if len(first) != 1 || !strings.Contains(string(first[0].Data), "[UTC]") ||
		!strings.Contains(string(first[0].Data), "Browser time zone for this request: unavailable") {
		t.Fatalf("first time context = %#v", first)
	}
	foundInModel := false
	for _, message := range model.observed[0] {
		foundInModel = foundInModel || strings.Contains(message.Content, "Time sampled while preparing turn 1, step 1:")
	}
	if !foundInModel {
		t.Fatal("the model did not receive the durable time snapshot")
	}
	if _, err := loop.RunTextTurn(context.Background(), "same clock window"); err != nil {
		t.Fatal(err)
	}
	if count := len(timeContextEvents(t, live.DurableEvents())); count != 1 {
		t.Fatalf("ten-minute refresh appended %d snapshots", count)
	}
	secondJournal := &nativeTestJournal{events: append([]sessionformat.Event(nil), live.DurableEvents()...)}
	restored, err := sessionformat.RestoreLiveSession(sessionformat.PhysicalArtifact{Header: header, Events: live.DurableEvents()}, secondJournal)
	if err != nil {
		t.Fatal(err)
	}
	resumeModel := &nativeInstructionModel{}
	resume, err := NewNativeLoop(restored, resumeModel, "provider", "model", "system", nil, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := resume.EnableTimeContext("UTC", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := resume.RunTextTurn(context.Background(), "after cold resume"); err != nil {
		t.Fatal(err)
	}
	if count := len(timeContextEvents(t, restored.DurableEvents())); count != 1 {
		t.Fatalf("cold resume forgot the last clock sample: %d", count)
	}
	if err := resume.EnableTimeContext("UTC", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := resume.RunTextTurn(context.Background(), "refresh now"); err != nil {
		t.Fatal(err)
	}
	if count := len(timeContextEvents(t, restored.DurableEvents())); count != 2 {
		t.Fatalf("zero refresh did not inject again: %d", count)
	}
	if err := sessionformat.ValidateV4Relationships(restored.DurableEvents()); err != nil {
		t.Fatalf("time context broke v4 relationships: %v", err)
	}
	if path := os.Getenv("SERVERAGENTS_V4_TIME_CONTEXT_EXPORT_PATH"); path != "" {
		if err := os.WriteFile(path, nativeTestJournalJSONL(t, header, secondJournal), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeTimeContextTimestampAndDuration(t *testing.T) {
	clock := &nativeTimeContext{zone: "Asia/Shanghai", location: time.FixedZone("Asia/Shanghai", 8*3600)}
	stamp := nativeTimeTimestamp(time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC), clock)
	if stamp != "2026-10-12T09:02:03+08:00[Asia/Shanghai]" {
		t.Fatalf("timestamp = %q", stamp)
	}
	if got := nativeTimeDuration(25*time.Hour + 2*time.Minute + 3*time.Second); got != "1d 1h 2m 3s" {
		t.Fatalf("elapsed = %q", got)
	}
}

func TestNativeTimeContextUsesRequestZoneAndDetectsMixedZones(t *testing.T) {
	for _, check := range []struct {
		name   string
		zones  []string
		wanted string
		stamp  string
	}{
		{name: "resolved", zones: []string{"Asia/Shanghai"}, wanted: "Browser time zone for this request: Asia/Shanghai. Interpret otherwise-unqualified dates and times in this zone.", stamp: "+08:00[Asia/Shanghai]"},
		{name: "mixed", zones: []string{"Asia/Shanghai", "America/New_York"}, wanted: `Browser time zone for this request: mixed ["America/New_York","Asia/Shanghai"]. Ask the user to clarify otherwise-unqualified dates and times.`, stamp: "[UTC]"},
	} {
		t.Run(check.name, func(t *testing.T) {
			header := sessionformat.Header{Type: "session", Version: 4, ID: "clock-zone", CreatedAt: 1}
			journal := &nativeTestJournal{}
			live, err := sessionformat.NewLiveSession(header, journal)
			if err != nil {
				t.Fatal(err)
			}
			loop, err := NewNativeLoop(live, &nativeInstructionModel{}, "provider", "model", "system", nil, 3, 3)
			if err != nil {
				t.Fatal(err)
			}
			if err := loop.EnableTimeContext("UTC", 10*time.Minute); err != nil {
				t.Fatal(err)
			}
			for index, zone := range check.zones {
				message, err := json.Marshal(map[string]any{
					"id": "message-" + zone, "role": "user", "source": map[string]any{"kind": "user", "rpcId": "request-" + zone, "clientTimeZone": zone},
					"content": []any{map[string]any{"type": "text", "text": "time request"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				if index == 0 {
					_, err = loop.AcceptMessage(context.Background(), message)
				} else {
					_, err = loop.SteerMessage(context.Background(), message)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := loop.RunNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			events := timeContextEvents(t, live.DurableEvents())
			var snapshot struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if len(events) != 1 {
				t.Fatalf("time context = %#v", events)
			}
			if err := json.Unmarshal(events[0].Data, &snapshot); err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Content) != 1 || !strings.Contains(snapshot.Content[0].Text, check.wanted) || !strings.Contains(snapshot.Content[0].Text, check.stamp) {
				t.Fatalf("time context = %#v", snapshot)
			}
			if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
				t.Fatal(err)
			}
			if path := os.Getenv("SERVERAGENTS_V4_TIME_ZONE_EXPORT_PATH"); path != "" && check.name == "resolved" {
				if err := os.WriteFile(path, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
