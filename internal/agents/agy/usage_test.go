package agy

import "testing"

func TestAgyModelFamily(t *testing.T) {
	tests := map[string]string{
		"gemini-3.8-flash-low":  "gemini",
		"Claude and GPT models": "third-party",
		"gpt-oss-120b-medium":   "third-party",
		"unknown-model":         "",
	}
	for input, want := range tests {
		if got := agyModelFamily(input); got != want {
			t.Errorf("agyModelFamily(%q) = %q, want %q", input, got, want)
		}
	}
}
