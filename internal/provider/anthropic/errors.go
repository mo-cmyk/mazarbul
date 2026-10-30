package anthropic

import "fmt"

// APIError is the structured error Anthropic returns on a non-2xx response.
// It implements the error interface and is errors.As-friendly so callers
// can branch on Type or StatusCode.
type APIError struct {
	// StatusCode is the HTTP status returned by the API (400, 401, 429, ...).
	StatusCode int

	// Type is Anthropic's error type tag, e.g., "authentication_error",
	// "rate_limit_error", "invalid_request_error", "overloaded_error",
	// "api_error". Empty when the response body did not decode.
	Type string

	// Message is the human-readable error message Anthropic returned.
	// Empty when the response body did not decode.
	Message string

	// RequestID is the value of the "request-id" response header, when
	// present. Useful for support tickets.
	RequestID string
}

// Error returns a single-line description of the error. It never includes
// the API key or any request body content.
func (e *APIError) Error() string {
	switch {
	case e.Type != "" && e.Message != "":
		return fmt.Sprintf("anthropic: %s (status %d, type %s)", e.Message, e.StatusCode, e.Type)
	case e.Message != "":
		return fmt.Sprintf("anthropic: %s (status %d)", e.Message, e.StatusCode)
	default:
		return fmt.Sprintf("anthropic: HTTP %d", e.StatusCode)
	}
}

// wireError is the JSON shape of an Anthropic error response. Private to
// the package; only the parser uses it.
type wireError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
