package panel

import (
	"testing"
	"time"
)

func TestIntervalParsingIsSafe(t *testing.T) {
	if got := intervalToTime(nil); got != 0 {
		t.Fatalf("nil interval = %s, want 0", got)
	}
	if got := intervalToTime("not-a-number"); got != 0 {
		t.Fatalf("invalid interval = %s, want 0", got)
	}
	if got := clampInterval(0, 30*time.Second, time.Hour); got != 30*time.Second {
		t.Fatalf("clamped zero interval = %s, want 30s", got)
	}
	if got := clampInterval(2*time.Hour, 30*time.Second, time.Hour); got != time.Hour {
		t.Fatalf("clamped large interval = %s, want 1h", got)
	}
}
