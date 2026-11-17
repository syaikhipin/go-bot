package tools

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"go-bot/web"
)

func TestPinnedDeepSeekWebParity(t *testing.T) {
	data, err := os.ReadFile("testdata/web-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string
			Tool     string
			Args     json.RawMessage
			Search   map[string]web.SearchResult
			Fetch    web.FetchResult
			Expected struct {
				Value json.RawMessage
				Text  string
				Meta  json.RawMessage
				Error string
			}
		}
	}
	if err = json.Unmarshal(data, &fixture); err != nil || len(fixture.Cases) == 0 {
		t.Fatalf("decode upstream fixture: %v", err)
	}
	for _, sample := range fixture.Cases {
		t.Run(sample.Name, func(t *testing.T) {
			var value any
			var text string
			var meta json.RawMessage
			var err error
			switch sample.Tool {
			case "web_search":
				tool := WebSearch{Searcher: &batchSearchStub{results: sample.Search, calls: make(map[string]int)}}
				value, err = tool.ExecuteValue(t.Context(), sample.Args)
				if err == nil {
					text, meta, err = tool.ExecutePresented(t.Context(), sample.Args)
				}
			case "web_fetch":
				tool := WebFetch{Fetcher: &fakeWebFetcher{result: sample.Fetch}}
				value, err = tool.ExecuteValue(t.Context(), sample.Args)
				if err == nil {
					text, meta, err = tool.ExecutePresented(t.Context(), sample.Args)
				}
			default:
				t.Fatalf("unexpected fixture tool %q", sample.Tool)
			}
			if sample.Expected.Error != "" {
				if err == nil || err.Error() != sample.Expected.Error {
					t.Fatalf("error = %v, want %q", err, sample.Expected.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if text != sample.Expected.Text {
				t.Errorf("model text mismatch\ngot:  %q\nwant: %q", text, sample.Expected.Text)
			}
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, data, sample.Expected.Value)
			assertJSONEqual(t, meta, sample.Expected.Meta)
		})
	}
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var a, b any
	if json.Unmarshal(got, &a) != nil || json.Unmarshal(want, &b) != nil || !reflect.DeepEqual(a, b) {
		t.Errorf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}
