package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// RFC 9116 requires an Expires field, and a stale security.txt tells
// researchers nobody is listening. This fails a month before it lapses.
func TestSecurityTxtNotExpiring(t *testing.T) {
	b, err := os.ReadFile("../public/.well-known/security.txt")
	if err != nil {
		t.Skip("security.txt not present")
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Expires: "); ok {
			exp, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
			if err != nil {
				t.Fatalf("Expires is not RFC 3339: %v", err)
			}
			if time.Until(exp) < 30*24*time.Hour {
				t.Errorf("security.txt expires %s; move Expires on by a year", exp.Format("2006-01-02"))
			}
			return
		}
	}
	t.Error("security.txt has no Expires field")
}
