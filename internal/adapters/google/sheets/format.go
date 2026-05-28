package sheets

import "strings"

func rangeSheetLabel(a1 string) string {
	// "Sheet1!A1:D10" -> "Sheet1"
	if i := strings.Index(a1, "!"); i > 0 {
		return a1[:i]
	}
	return a1
}
