package winname

import "testing"

func TestCannotHold(t *testing.T) {
	for name, want := range map[string]bool{
		"Pilot Validation ": true, "report.": true, "a. ": true, "x\t": false,
		"Pilot Validation": false, ".hidden": false, "a.b": false, " lead": false,
		".": false, "..": false, "": false,
	} {
		if got := CannotHold(name); got != want {
			t.Errorf("CannotHold(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPathCannotHold(t *testing.T) {
	for p, want := range map[string]bool{
		"a/Pilot Validation /doc.pdf": true, "a/b/c.": true,
		"a/Pilot Validation/doc.pdf": false, "": false,
	} {
		if got := PathCannotHold(p); got != want {
			t.Errorf("PathCannotHold(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestEquivalent(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"SL/Pilot Validation /new.pdf", "SL/Pilot Validation/new.pdf", true},
		{"SL/pilot validation/x", "SL/Pilot Validation/x", true},
		{"a/report.", "a/report", true},
		{"a/x/new.pdf", "a/y/new.pdf", false},
		{"a/Pilot Validation/new.pdf", "a/Pilot Validationn/new.pdf", false},
	} {
		if got := Equivalent(c.a, c.b); got != c.want {
			t.Errorf("Equivalent(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
