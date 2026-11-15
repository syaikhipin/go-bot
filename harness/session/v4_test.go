package sessionformat

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCurrentDeepSeekHeaderShape(t *testing.T) {
	// Shape and field names are from the official v4 Session fixtures.
	line := []byte(`{"type":"session","version":4,"id":"root-id","createdAt":1783352050748,"cwd":"/workspace","isSeeded":false,"delegationDepth":0}`)
	header, err := DecodeHeader(line)
	if err != nil || header.ID != "root-id" || header.Version != Version {
		t.Fatalf("v4 header = %#v, %v", header, err)
	}
	encoded, err := EncodeHeader(header)
	if err != nil || string(encoded) != string(line) {
		t.Fatalf("v4 header roundtrip = %s, %v", encoded, err)
	}
	for _, bad := range []string{
		`{"type":"session","version":4,"id":"root-id","createdAt":0,"delegationDepth":0}`,
		`{"type":"session","version":4,"id":"root-id","createdAt":0,"isSeeded":false,"delegationDepth":0,"userId":"alice"}`,
		`{"type":"session","version":3,"id":"root-id","createdAt":0,"isSeeded":false,"delegationDepth":0}`,
		`{"type":"session","version":4,"id":"root-id","createdAt":0,"cwd":"","isSeeded":false,"delegationDepth":0}`,
		`{"type":"session","version":4,"id":"root-id","createdAt":0,"isSeeded":false,"origin":"","delegationDepth":0}`,
	} {
		if _, err := DecodeHeader([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid v4 header: %s", bad)
		}
	}
}

func TestCurrentDeepSeekEventEnvelope(t *testing.T) {
	line := []byte(`{"type":"turn/start","seq":0,"time":1783352050748,"data":{"turn":1}}`)
	event, err := DecodeEvent(line, 0)
	if err != nil || event.Type != "turn/start" || event.Seq != 0 {
		t.Fatalf("v4 event = %#v, %v", event, err)
	}
	encoded, err := EncodeEvent(event, 0)
	if err != nil || string(encoded) != string(line) {
		t.Fatalf("v4 event roundtrip = %s, %v", encoded, err)
	}
	for _, bad := range []string{
		`{"type":"turn/start","seq":1,"time":1783352050748,"data":{"turn":1}}`,
		`{"type":"turn/start","seq":0,"time":1783352050748,"data":{"turn":1},"kind":"event"}`,
		`{"type":"turn/start","seq":0,"time":1783352050748}`,
	} {
		if _, err := DecodeEvent([]byte(bad), 0); err == nil {
			t.Fatalf("accepted invalid v4 envelope: %s", bad)
		}
	}
	if !json.Valid(event.Data) || strings.Contains(string(encoded), `"at"`) {
		t.Fatalf("wrong v4 event encoding: %s", encoded)
	}
}

func TestReleasedV4SurfaceMetadata(t *testing.T) {
	for _, line := range []string{
		`{"type":"system/message","seq":0,"time":1,"data":{},"surfaceOp":"append"}`,
		`{"type":"developer/message","seq":1,"time":1,"data":{},"surfaceOp":"append"}`,
		`{"type":"tool/result","seq":2,"time":1,"data":{},"surfaceOp":"append","sourceEventSeqs":[0,1]}`,
		`{"type":"tool/result","seq":5,"time":1,"data":{},"surfaceOp":"append","sourceEventSeqs":[[0,2],4]}`,
		`{"type":"user/message","seq":2,"time":1,"data":{},"surfaceOp":{"op":"replace","startSeq":0,"endSeq":1},"sourceEventSeqs":[0]}`,
		`{"type":"external/opaque","seq":2,"time":1,"data":null,"surfaceOp":{"future":true},"sourceEventSeqs":[0],"ignorable":true}`,
	} {
		var raw struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeEvent([]byte(line), raw.Seq); err != nil {
			t.Errorf("rejected v4 surface row %s: %v", line, err)
		}
	}
	for _, line := range []string{
		`{"type":"system/message","seq":0,"time":1,"data":{}}`,
		`{"type":"assistant/message","seq":1,"time":1,"data":{},"surfaceOp":"append","sourceEventSeqs":[0]}`,
		`{"type":"tool/result","seq":1,"time":1,"data":{},"surfaceOp":"append","sourceEventSeqs":[1]}`,
		`{"type":"tool/result","seq":2,"time":1,"data":{},"surfaceOp":"append","sourceEventSeqs":[0,0]}`,
		`{"type":"tool/result","seq":5,"time":1,"data":{},"surfaceOp":"append","sourceEventSeqs":[[0,2],2]}`,
		`{"type":"tool/result","seq":5,"time":1,"data":{},"surfaceOp":"append","sourceEventSeqs":[[0,3],[2,4]]}`,
		`{"type":"tool/result","seq":5,"time":1,"data":{},"surfaceOp":"append","sourceEventSeqs":[[0,5]]}`,
		`{"type":"tool/result","seq":5,"time":1,"data":{},"surfaceOp":"append","sourceEventSeqs":[[0]]}`,
		`{"type":"tool/result","seq":5,"time":1,"data":{},"surfaceOp":"append","sourceEventSeqs":[null]}`,
		`{"type":"user/message","seq":1,"time":1,"data":{},"surfaceOp":{"op":"replace","startSeq":0,"endSeq":1}}`,
		`{"type":"user/message","seq":1,"time":1,"data":{},"surfaceOp":{"op":"replace","startSeq":null,"endSeq":0}}`,
		`{"type":"user/message","seq":0,"time":1,"data":{},"surfaceOp":{"op":"replace","startSeq":0,"endSeq":0}}`,
		`{"type":"turn/start","seq":1,"time":1,"data":{"turn":1},"surfaceOp":"append"}`,
		`{"type":"external/opaque","seq":1,"time":1,"data":{},"sourceEventSeqs":[]}`,
		`{"type":"external/opaque","seq":1,"time":1,"data":{},"sourceEventSeqs":null,"ignorable":true}`,
	} {
		var raw struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeEvent([]byte(line), raw.Seq); err == nil {
			t.Errorf("accepted invalid v4 surface row: %s", line)
		}
	}
}

func TestReleasedV4NumberAdmission(t *testing.T) {
	header, err := DecodeHeader([]byte(`{"type":"session","version":4.0,"id":"","createdAt":1e0,"isSeeded":false,"delegationDepth":0.0}`))
	if err != nil || header.ID != "" || header.CreatedAt != 1 {
		t.Fatalf("released numeric header = %#v, %v", header, err)
	}
	event, err := DecodeEvent([]byte(`{"type":"turn/start","seq":0e0,"time":-1e0,"data":{"turn":1}}`), 0)
	if err != nil || event.Time != -1 {
		t.Fatalf("released numeric event = %#v, %v", event, err)
	}
	for _, line := range []string{
		`{"type":"session","version":4,"id":"id","createdAt":-0,"isSeeded":false,"delegationDepth":0}`,
		`{"type":"session","version":4,"id":"id","createdAt":0,"isSeeded":false,"delegationDepth":-0}`,
		`{"type":"session","version":4,"id":"id","createdAt":9007199254740992,"isSeeded":false,"delegationDepth":0}`,
	} {
		if _, err := DecodeHeader([]byte(line)); err == nil {
			t.Errorf("accepted invalid header number: %s", line)
		}
	}
	for _, line := range []string{
		`{"type":"turn/start","seq":-0,"time":1,"data":{}}`,
		`{"type":"turn/start","seq":0,"time":-0,"data":{}}`,
		`{"type":"turn/start","seq":0,"time":1.5,"data":{}}`,
		`{"type":"turn/start","seq":0,"time":9007199254740992,"data":{}}`,
	} {
		if _, err := DecodeEvent([]byte(line), 0); err == nil {
			t.Errorf("accepted invalid event number: %s", line)
		}
	}
}

func TestEncodeSourceSeqsMatchesReleasedRangeEncoding(t *testing.T) {
	for _, test := range []struct {
		seqs []uint64
		want string
	}{
		{[]uint64{0}, `[0]`},
		{[]uint64{0, 1}, `[0,1]`},
		{[]uint64{0, 1, 2, 4, 5}, `[[0,2],4,5]`},
		{[]uint64{3, 1, 2}, `[3,1,2]`},
	} {
		got, err := EncodeSourceSeqs(test.seqs, 6)
		if err != nil || string(got) != test.want {
			t.Fatalf("EncodeSourceSeqs(%v) = %s, %v; want %s", test.seqs, got, err, test.want)
		}
	}
	for _, seqs := range [][]uint64{{}, {0, 0}, {6}, {0, 7}} {
		if _, err := EncodeSourceSeqs(seqs, 6); err == nil {
			t.Fatalf("accepted invalid logical sources: %v", seqs)
		}
	}
}
