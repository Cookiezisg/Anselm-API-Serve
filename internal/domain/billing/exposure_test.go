package billing

import "testing"

func TestChargeExposureDefaultsToConservative(t *testing.T) {
	if !ChargeExposure(0).MayHaveCharged() {
		t.Fatal("zero-value exposure must retain the reservation")
	}
	if DefinitelyUnbilled.MayHaveCharged() {
		t.Fatal("a definitely-unbilled outcome must permit rollback")
	}
	if !ChargeExposure(255).MayHaveCharged() {
		t.Fatal("unknown exposure values must fail safe")
	}
}
