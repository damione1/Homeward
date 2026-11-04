package apitypes

import (
	"errors"
	"strconv"
)

// DecimalUint64 preserves a native full-width position without lexical ordering.
// Its wire form is base ten, with no sign, whitespace or redundant leading zero.
type DecimalUint64 struct{ value uint64 }

func ParseDecimalUint64(text string) (DecimalUint64, error) {
	if len(text) == 0 || len(text) > 20 || (len(text) > 1 && text[0] == '0') {
		return DecimalUint64{}, errors.New("invalid canonical decimal position")
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return DecimalUint64{}, errors.New("invalid canonical decimal position")
		}
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return DecimalUint64{}, errors.New("invalid canonical decimal position")
	}
	return DecimalUint64{value}, nil
}
func DecimalUint64FromNative(value uint64) DecimalUint64 { return DecimalUint64{value} }
func (value DecimalUint64) String() string               { return strconv.FormatUint(value.value, 10) }
func (value DecimalUint64) Value() uint64                { return value.value }
