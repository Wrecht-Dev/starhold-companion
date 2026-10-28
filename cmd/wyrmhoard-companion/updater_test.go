package main

import "testing"

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.1", "0.1.0", 1},
		{"0.1.0", "0.1.1", -1},
		{"0.1.0", "0.1.0", 0},
		{"1.0.0", "0.9.9", 1},
		{"0.2.0", "0.10.0", -1}, // numeric, not lexicographic
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestUpdatesDisabled(t *testing.T) {
	t.Setenv("WYRMHOARD_COMPANION_NO_UPDATE", "1")
	if !updatesDisabled() {
		t.Fatal("env flag should disable updates")
	}
	t.Setenv("WYRMHOARD_COMPANION_NO_UPDATE", "")
	if updatesDisabled() {
		t.Fatal("empty flag should not disable updates")
	}
}
