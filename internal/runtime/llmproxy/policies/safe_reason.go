package policies

// ModelSafeInternalReason returns text that can be shown to the model when an
// internal dependency failed. Raw Go/provider/store errors belong in audit
// fields and logs, not assistant-visible policy reasons.
func ModelSafeInternalReason(action string) string {
	if action == "" {
		action = "request"
	}
	return "Clawvisor: " + action + " failed; details are in the Clawvisor audit log."
}

// ModelSafeUnavailableReason returns text for unavailable proxy services such
// as approval storage or nonce minting.
func ModelSafeUnavailableReason(service string) string {
	if service == "" {
		service = "service"
	}
	return "Clawvisor: " + service + " unavailable; details are in the Clawvisor audit log."
}
