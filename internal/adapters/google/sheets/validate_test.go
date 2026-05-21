package sheets

import "testing"

func TestRequireSpreadsheetID(t *testing.T) {
	_, err := requireSpreadsheetID(map[string]any{})
	if err == nil {
		t.Fatalf("expected error")
	}

	_, err = requireSpreadsheetID(map[string]any{"spreadsheet_id": "!!!"})
	if err == nil {
		t.Fatalf("expected invalid id error")
	}

	got, err := requireSpreadsheetID(map[string]any{"spreadsheet_id": "1AbcdefGhij-klmNOPQr"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "1AbcdefGhij-klmNOPQr" {
		t.Fatalf("got %q", got)
	}
}

func TestRequireA1Range(t *testing.T) {
	_, err := requireA1Range(map[string]any{})
	if err == nil {
		t.Fatalf("expected error")
	}
	_, err = requireA1Range(map[string]any{"range": "A1:B2"})
	if err == nil {
		t.Fatalf("expected missing sheet name error")
	}
	_, err = requireA1Range(map[string]any{"range": "Sheet1!A1\nB2"})
	if err == nil {
		t.Fatalf("expected invalid range error")
	}
	got, err := requireA1Range(map[string]any{"range": "Sheet1!A1:D10"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Sheet1!A1:D10" {
		t.Fatalf("got %q", got)
	}
}

func TestRequire2DValues(t *testing.T) {
	_, err := require2DValues(map[string]any{}, "rows")
	if err == nil {
		t.Fatalf("expected error")
	}
	_, err = require2DValues(map[string]any{"rows": "nope"}, "rows")
	if err == nil {
		t.Fatalf("expected error")
	}
	_, err = require2DValues(map[string]any{"rows": []any{"nope"}}, "rows")
	if err == nil {
		t.Fatalf("expected error")
	}

	rows, err := require2DValues(map[string]any{"rows": []any{[]any{"a", 1, true}}}, "rows")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) != 3 {
		t.Fatalf("unexpected rows: %#v", rows)
	}
}
