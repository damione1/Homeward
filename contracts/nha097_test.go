package contracts

import (
	"math"
	"testing"
	"time"

	helperwire "github.com/damione1/Homeward/gen/home/helpers/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA097HelperWirePreservesPresentZeroAndCivilKinds(t *testing.T) {
	date, _ := domain.NewCivilDate(2026, 9, 1)
	clock, _ := domain.NewCivilTime(0, 0, 0)
	civil, _ := domain.NewCivilDateTime(date, clock)
	dateValue, _ := domain.NewHelperDate(date)
	timeValue, _ := domain.NewHelperTime(clock)
	datetimeValue, _ := domain.NewHelperDateTime(civil)
	textValue, _ := domain.NewHelperText("")
	values := []domain.HelperValue{domain.NewHelperBoolean(false), domain.NewHelperCounter(0), dateValue, timeValue, datetimeValue, textValue}
	for _, want := range values {
		encoded, err := encodeHelperWire(want, 1)
		if err != nil || encoded.Value == nil || encoded.HelperRevision != 1 {
			t.Fatalf("present helper lost on encoding: %v", err)
		}
		got, revision, err := admitHelperWire(encoded, true)
		if err != nil || revision != 1 || got != want {
			t.Fatalf("helper round-trip drift: %v", err)
		}
		encoded.HelperRevision = 0
		if _, _, err := admitHelperWire(encoded, true); err == nil {
			t.Fatal("present observation admitted without SQL revision")
		}
	}
}

func TestNHA097HelperWireRejectsUnknownAndLossyPayloads(t *testing.T) {
	valid := &helperwire.HelperValue{Value: &helperwire.HelperValue_Boolean{Boolean: false}, HelperRevision: 1}
	valid.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	button := timestamppb.New(time.Date(2026, 10, 18, 12, 0, 0, 1, time.UTC))
	for _, value := range []*helperwire.HelperValue{
		nil,
		{},
		valid,
		{Value: (*helperwire.HelperValue_Date)(nil), HelperRevision: 1},
		{Value: &helperwire.HelperValue_Date{Date: &helperwire.CivilDate{Year: 2026, Month: 2, Day: 30}}, HelperRevision: 1},
		{Value: &helperwire.HelperValue_Time{Time: &helperwire.CivilTime{Hour: 24}}, HelperRevision: 1},
		{Value: &helperwire.HelperValue_ButtonPressedAt{ButtonPressedAt: button}, HelperRevision: 1},
		{Value: &helperwire.HelperValue_Boolean{Boolean: false}, HelperRevision: math.MaxUint64},
	} {
		if _, _, err := admitHelperWire(value, true); err == nil {
			t.Fatal("invalid helper observation wire admitted")
		}
	}
	if _, err := AdmitHelperSetValue(&helperwire.HelperValue{Value: &helperwire.HelperValue_Text{Text: ""}, HelperRevision: 1}); err == nil {
		t.Fatal("set payload carried an observation revision")
	}
}
