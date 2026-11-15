package localstate

import "testing"

func TestExclusiveControllerAndRelease(t *testing.T) {
	dir := t.TempDir()
	a, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	if b, e := Open(dir); e == nil {
		b.Close()
		t.Fatal("second controller acquired lock")
	}
	a.Close()
	b, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	b.Close()
}
