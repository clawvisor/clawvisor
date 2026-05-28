package sheets

import "testing"

func TestRangeSheetLabel(t *testing.T) {
	if got := rangeSheetLabel("Sheet1!A1:D10"); got != "Sheet1" {
		t.Fatalf("got %q", got)
	}
	if got := rangeSheetLabel("NoBang"); got != "NoBang" {
		t.Fatalf("got %q", got)
	}
}
