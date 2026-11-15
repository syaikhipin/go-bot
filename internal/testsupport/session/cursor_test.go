package session

import (
	"context"
	"math"
	"testing"
)

func TestDurableEventsAfterUsesCursorAndReturnsCopiedBoundedPage(t *testing.T) {
	ctx := context.Background()
	header := testHeader()
	journal, err := (FileStore{Root: t.TempDir(), Claim: (&ProcessClaims{}).Acquire}).Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for index := 0; index < 4; index++ {
		if _, err := s.Append("skill/catalog", 0, 0, map[string]string{"text": "page"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := s.DurableEventsAfter(0, 2, 1, 1<<20)
	if err != nil || len(first) != 1 || first[0].Seq != 1 {
		t.Fatalf("first bounded cursor page = %#v, %v", first, err)
	}
	second, err := s.DurableEventsAfter(1, 2, 1<<20, 1<<20)
	if err != nil || len(second) != 2 || second[0].Seq != 2 || second[1].Seq != 3 {
		t.Fatalf("second cursor page = %#v, %v", second, err)
	}
	first[0].Data[0] = 'x'
	again, err := s.DurableEventsAfter(0, 1, 1<<20, 1<<20)
	if err != nil || again[0].Data[0] == 'x' {
		t.Fatalf("cursor page aliased live Session: %#v, %v", again, err)
	}
	if empty, err := s.DurableEventsAfter(math.MaxUint64, 2, 1<<20, 1<<20); err != nil || len(empty) != 0 {
		t.Fatalf("future cursor = %#v, %v", empty, err)
	}
}
