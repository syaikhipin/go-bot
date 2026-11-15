package session

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPlanProjectionUsesOnlyDurableLastSelection(t *testing.T) {
	header := appendStoreHeader()
	record, err := New(header, activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := record.Append("plan/mode", 0, 0, PlanMode{Active: true}); err != nil {
		t.Fatal(err)
	}
	if mode, err := record.CurrentPlanMode(); err != nil || mode.Active {
		t.Fatalf("unflushed mode leaked: %#v, %v", mode, err)
	}
	if err := record.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mode, err := record.CurrentPlanMode(); err != nil || !mode.Active {
		t.Fatalf("active mode = %#v, %v", mode, err)
	}
	if _, err := record.Append("plan/mode", 0, 0, PlanMode{Active: false}); err != nil {
		t.Fatal(err)
	}
	if err := record.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumed, err := Resume(header, record.DurableEvents(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.ValidatePlan(); err != nil {
		t.Fatal(err)
	}
	if mode, err := resumed.CurrentPlanMode(); err != nil || mode.Active {
		t.Fatalf("resumed mode = %#v, %v", mode, err)
	}
}

func TestPlanProjectionRejectsMalformedSelection(t *testing.T) {
	for _, raw := range []string{`{}`, `{"active":null}`, `{"active":"true"}`, `{"active":true,"other":1}`} {
		record, err := New(appendStoreHeader(), activityJournal{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := record.Append("plan/mode", 0, 0, json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
		if err := record.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := record.ValidatePlan(); err == nil {
			t.Fatalf("invalid plan mode %s was accepted", raw)
		}
	}
}
