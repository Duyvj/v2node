package panel

import (
	"regexp"
	"strings"
)

var secretQueryPattern = regexp.MustCompile(`(?i)([?&](?:token|api[_-]?key|apikey|key)=)[^&[:space:]]+`)

type redactedError struct {
	cause   error
	message string
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

// redactError keeps errors.Is/As behavior while preventing panel credentials
// embedded in REST URLs from reaching service logs.
func redactError(err error, token string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if token != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	message = secretQueryPattern.ReplaceAllString(message, `${1}[REDACTED]`)
	if message == err.Error() {
		return err
	}
	return &redactedError{cause: err, message: message}
}

var _ error = (*redactedError)(nil)
var _ interface{ Unwrap() error } = (*redactedError)(nil)
