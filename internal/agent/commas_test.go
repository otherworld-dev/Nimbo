package agent

import "testing"

// Counts in the status line are read at a glance from a narrow, truncating
// flyout label; a bare run of six digits is not.
func TestCommas(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{1204, "1,204"},
		{34120, "34,120"},
		{100000, "100,000"},
		{332000, "332,000"},
		{1000000, "1,000,000"},
		{1234567, "1,234,567"},
		{-4500, "-4,500"},
	}
	for _, c := range cases {
		if got := commas(c.in); got != c.want {
			t.Errorf("commas(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
