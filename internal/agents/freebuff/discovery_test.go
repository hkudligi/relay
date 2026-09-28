package freebuff

import "testing"

func TestParseFreebuffBalance(t *testing.T) {
	remaining, ok := parseFreebuffBalance("100/100 Freebucks remaining")
	if !ok || remaining != 100 {
		t.Fatalf("balance = %v, ok=%v", remaining, ok)
	}
	remaining, ok = parseFreebuffBalance("\x1b[38;2;241;245;249m42/100 Freebucks remaining")
	if !ok || remaining != 42 {
		t.Fatalf("ANSI balance = %v, ok=%v", remaining, ok)
	}
}

func TestParseFreebuffBalanceRejectsMissingOrInvalidLimits(t *testing.T) {
	for _, value := range []string{"Freebucks remaining", "0/0 Freebucks remaining"} {
		if _, ok := parseFreebuffBalance(value); ok {
			t.Fatalf("parseFreebuffBalance(%q) unexpectedly succeeded", value)
		}
	}
	remaining, ok := parseFreebuffBalance("101/100 Freebucks remaining")
	if !ok || remaining != 100 {
		t.Fatalf("clamped balance = %v, ok=%v", remaining, ok)
	}
}
