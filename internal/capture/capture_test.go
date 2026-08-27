package capture

import "testing"

func TestParsePageCount(t *testing.T) {
	pages, err := parsePageCount("Title: x\nPages:          12\nEncrypted: no\n")
	if err != nil {
		t.Fatal(err)
	}
	if pages != 12 {
		t.Fatalf("got %d", pages)
	}
}
