package userfacing

import "testing"

func TestZHEN(t *testing.T) {
	t.Parallel()
	tests := []struct {
		zh, en, want string
	}{
		{"余额不足", "insufficient balance", "余额不足 / insufficient balance"},
		{"  余额不足  ", " insufficient balance ", "余额不足 / insufficient balance"},
		{"same", "same", "same"},
		{"", "only en", "only en"},
		{"only zh", "", "only zh"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := ZHEN(tc.zh, tc.en); got != tc.want {
			t.Fatalf("ZHEN(%q, %q)=%q want %q", tc.zh, tc.en, got, tc.want)
		}
	}
}
