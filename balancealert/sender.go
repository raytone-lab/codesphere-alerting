package balancealert

// Sender delivers one balance-alert decision. OFF mode is handled by Runner
// before calling Send; implementations only see PILOT/CUSTOMER.
type Sender interface {
	Send(req SendRequest) SendResult
}

// HTTPSender is the legacy path: DingTalk robot webhook URLs + SMS webhook.
// Kept for tests and as a fallback when Nightingale notify rules are unset.
type HTTPSender struct {
	HTTP HTTPClient
}

func (s HTTPSender) Send(req SendRequest) SendResult {
	return Dispatch(s.HTTP, req)
}
