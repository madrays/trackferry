package names

import "testing"

// A song's name as a platform lists it may differ by a character from what people call it.
func TestNearTitle(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"雨中的恋人", "雨中的恋人们", true},   // the extra 们 a platform lists
		{"雨中的恋人们", "雨中的恋人", true},   // and the other way round
		{"海阔天空", "海阔天空", true},      // the same
		{"海阔天空", "海阔天", false},       // four characters: too short to guess
		{"情人", "爱人", false},          // one of three: another song
		{"雨中的城市", "雨中的恋人", false},   // four apart
	} {
		if got := NearTitle(tc.a, tc.b); got != tc.want {
			t.Errorf("NearTitle(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
