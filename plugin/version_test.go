package plugin

import "testing"

func TestNewerVersionUsesSemanticPrecedence(t *testing.T) {
	for _, tc := range []struct{ installed, available, want string }{
		{"0.9.0", "0.10.0", "0.10.0"}, {"0.10.0", "0.9.0", ""},
		{"1.0.0-rc.1", "1.0.0", "1.0.0"}, {"1.0.0", "1.0.0-rc.1", ""},
		{"1.0.0+one", "1.0.0+two", ""}, {"invalid", "1.0.0", ""},
	} {
		if got := NewerVersion(tc.installed, tc.available); got != tc.want {
			t.Errorf("%s -> %s: got %q, want %q", tc.installed, tc.available, got, tc.want)
		}
	}
}
