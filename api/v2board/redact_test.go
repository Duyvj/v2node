package panel

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactErrorPreservesCause(t *testing.T) {
	cause := errors.New("Get https://panel.example/api?node_id=1&token=secret-token: timeout")
	got := redactError(cause, "secret-token")
	if got == nil || got.Error() == cause.Error() {
		t.Fatal("error was not redacted")
	}
	if want := "token=[REDACTED]"; !strings.Contains(got.Error(), want) {
		t.Fatalf("redacted error %q does not contain %q", got.Error(), want)
	}
	if !errors.Is(got, cause) {
		t.Fatal("redaction did not preserve error cause")
	}
}
