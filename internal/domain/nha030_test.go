package domain

import (
	"errors"
	"testing"
)

func TestNHA030UnitBaseTransformIsAdmittedSnapshot(t *testing.T) {
	if _, _, err := (Unit{}).BaseTransform(); !errors.Is(err, ErrInvalidUnit) {
		t.Fatalf("zero unit admitted: %v", err)
	}
	catalog := NewUnitCatalog()
	if err := catalog.Register("custom-affine", DimensionTemperature, 2.5, 17); err != nil {
		t.Fatal(err)
	}
	unit, err := catalog.Parse("custom-affine")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register("other-affine", DimensionTemperature, 9, -3); err != nil {
		t.Fatal(err)
	}
	scale, offset, err := unit.BaseTransform()
	if err != nil || scale != 2.5 || offset != 17 {
		t.Fatalf("unit transform changed after catalogue growth: scale=%v offset=%v err=%v", scale, offset, err)
	}
}
