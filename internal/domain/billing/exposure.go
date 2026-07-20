package billing

// ChargeExposure describes what the gateway can prove about a provider charge
// when a call fails before any response body is handed to the application.
//
// ChargePossible is deliberately the zero value: an omitted or unknown value
// must retain the reservation. DefinitelyUnbilled is used only when the call
// never reached a provider or the provider returned an explicit pre-generation
// refusal (for example 401, 402, 400, or 429).
type ChargeExposure uint8

const (
	ChargePossible ChargeExposure = iota
	DefinitelyUnbilled
)

// MayHaveCharged is fail-safe for unknown future enum values: only the one
// explicit proof state permits a refund.
func (e ChargeExposure) MayHaveCharged() bool { return e != DefinitelyUnbilled }
