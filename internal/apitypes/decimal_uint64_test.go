package apitypes

import (
	"math"
	"testing"
)

func TestDecimalUint64CanonicalFullWidth(t *testing.T) {
	for _, n := range []uint64{0, 9, 10, math.MaxInt64, math.MaxUint64} {
		text := DecimalUint64FromNative(n).String()
		got, err := ParseDecimalUint64(text)
		if err != nil || got.Value() != n {
			t.Fatalf("decimal round trip %s: %v", text, err)
		}
	}
	for _, text := range []string{"", "00", "01", "-1", "+1", " 1", "1 ", "1.0", "18446744073709551616"} {
		if _, err := ParseDecimalUint64(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
	nine, _ := ParseDecimalUint64("9")
	ten, _ := ParseDecimalUint64("10")
	if nine.Value() >= ten.Value() {
		t.Fatal("lexical instead of numeric order")
	}
}
