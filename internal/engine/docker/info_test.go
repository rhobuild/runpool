package docker

import (
	"strings"
	"testing"
)

func TestParseSwapTotal(t *testing.T) {
	got, err := parseSwapTotal(strings.NewReader("MemTotal: 16384 kB\nSwapTotal: 4096 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 4<<20 {
		t.Fatalf("swap total = %d; want %d", got, 4<<20)
	}
}

func TestParseSwapTotalRejectsMissingOrMalformedValues(t *testing.T) {
	for _, input := range []string{
		"MemTotal: 16384 kB\n",
		"SwapTotal: unknown kB\n",
		"SwapTotal: 4096 MB\n",
	} {
		if _, err := parseSwapTotal(strings.NewReader(input)); err == nil {
			t.Errorf("parseSwapTotal(%q) succeeded; want error", input)
		}
	}
}
