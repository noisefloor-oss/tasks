package envcompat

import (
	"strings"
	"testing"
)

func TestLookup(t *testing.T) {
	const canonical = "NOISE_TASKS_TEST_VALUE"
	const legacy = "NOISETASK_TEST_VALUE"
	tests := []struct {
		name      string
		canonical string
		legacy    string
		want      string
		wantErr   bool
	}{
		{name: "unset"},
		{name: "canonical", canonical: "new", want: "new"},
		{name: "legacy", legacy: "old", want: "old"},
		{name: "equal", canonical: "same", legacy: "same", want: "same"},
		{name: "conflict", canonical: "new", legacy: "old", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(canonical, test.canonical)
			t.Setenv(legacy, test.legacy)
			got, err := Lookup(canonical, legacy)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), canonical) || !strings.Contains(err.Error(), legacy) {
					t.Fatalf("Lookup() error = %v, want conflict naming both variables", err)
				}
				if strings.Contains(err.Error(), "new") || strings.Contains(err.Error(), "old") {
					t.Fatalf("Lookup() error leaks values: %v", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("Lookup() = %q, %v; want %q, nil", got, err, test.want)
			}
		})
	}
}
