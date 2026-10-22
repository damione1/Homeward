package ecobee

import (
	"github.com/damione1/Homeward/internal/ecobee/ecobeetest"
	"testing"
)

type ordinaryFixture = ecobeetest.Fixture

func newOrdinaryFixture(t *testing.T) *ordinaryFixture { return ecobeetest.New(t) }
