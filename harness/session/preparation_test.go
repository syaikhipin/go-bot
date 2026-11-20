package sessionformat

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// Expected rows were produced by the original published dsh-session
// 0.2.2-rc.4 SessionStore.prepare, not by this Go implementation. The one-off
// oracle/provenance lives in the ignored plugin-framework QA directory.
func TestPreparedSessionMatchesOriginalConstructorCases(t *testing.T) {
	var scenarios []struct {
		Name      string  `json:"name"`
		IsSeeded  bool    `json:"isSeeded"`
		Inherited *uint64 `json:"inheritedEventCount"`
		Seed      []Event `json:"seed"`
	}
	var expected []struct {
		Name      string  `json:"name"`
		OK        bool    `json:"ok"`
		Inherited uint64  `json:"inheritedEventCount"`
		Events    []Event `json:"events"`
	}
	read := func(path string, target any) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	read("testdata/preparation-cases.json", &scenarios)
	read("testdata/preparation-upstream.json", &expected)
	if len(scenarios) != len(expected) {
		t.Fatal("oracle cases are incomplete")
	}
	for index, scenario := range scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			want := expected[index]
			if scenario.Name != want.Name {
				t.Fatal("oracle case order differs")
			}
			header := Header{Type: "session", Version: 4, ID: scenario.Name, CreatedAt: 1, CWD: "/workspace", IsSeeded: scenario.IsSeeded}
			prepared, err := prepareSessionAt(header, scenario.Seed, scenario.Inherited, 1000)
			if want.OK != (err == nil) {
				t.Fatal("upstream acceptance differs", err)
			}
			if err != nil {
				return
			}
			if prepared.session.InheritedEventCount() != want.Inherited {
				t.Fatal("inherited cut differs")
			}
			if position := prepared.session.Position(); position.Seq != uint64(len(want.Events)) || position.InheritedEventCount != want.Inherited {
				t.Fatal("synchronous Session position differs from original constructor", position)
			}
			normalize := func(events []Event) any {
				data, _ := json.Marshal(events)
				var value any
				if err := json.Unmarshal(data, &value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			if !reflect.DeepEqual(normalize(prepared.Events()), normalize(want.Events)) {
				t.Fatal("constructor events differ from upstream", prepared.Events())
			}
			if prepared.Header() != header {
				t.Fatal("preparation changed immutable metadata")
			}
			if len(scenario.Seed) > 0 {
				scenario.Seed[0].Data = json.RawMessage(`{"caller":"changed"}`)
				if reflect.DeepEqual(prepared.Events()[0].Data, scenario.Seed[0].Data) {
					t.Fatal("caller retained mutable seed ownership")
				}
			}
		})
	}
}

func TestPreparedSeedIsPublishedAtomicallyAndAttachedOnlyOnce(t *testing.T) {
	store := newLiveFileStore(t)
	header := v4TestHeader("prepared-seed")
	header.CWD = store.ProjectCWD
	header.IsSeeded = true
	count := uint64(1)
	seed := []Event{{Type: "permission/preset", Seq: 0, Time: 1, Data: json.RawMessage(`{"preset":"read-only"}`)}}
	prepared, err := PrepareSession(header, seed, &count)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := store.CreatePrepared(t.Context(), header, prepared.Events())
	if err != nil {
		t.Fatal(err)
	}
	// Read the canonical file before attaching the live writer. The full seed
	// and inherited marker must already be present, rather than only a header.
	artifact, err := store.ReadArtifact(t.Context(), header.ID)
	if err != nil || artifact.InheritedEventCount != 1 || len(artifact.Events) != 2 {
		t.Fatal("partial seed publication", artifact, err)
	}
	live, err := prepared.Attach(journal, len(artifact.Events))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := prepared.Attach(journal, len(artifact.Events)); err == nil {
		t.Fatal("writer attached twice")
	}
	if _, err := live.Append(EventInput{Type: "approval/policy", Data: json.RawMessage(`{"policy":"ask"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := live.Flush(t.Context()); err != nil {
		t.Fatal("attached writer duplicated constructor events", err)
	}
	artifact, err = store.ReadArtifact(t.Context(), header.ID)
	if err != nil || len(artifact.Events) != 3 || artifact.Events[2].Type != "approval/policy" {
		t.Fatal("prepared writer cursor differs from the journal", artifact, err)
	}
	if _, err := store.CreatePrepared(t.Context(), header, prepared.Events()); err == nil {
		t.Fatal("existing journal replaced")
	}
}
