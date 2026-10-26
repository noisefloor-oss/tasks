package backlog

import (
	"math"
	"testing"
)

func TestCheckedSpliceSize(t *testing.T) {
	for _, tt := range []struct {
		name    string
		kept    int
		added   []int
		want    int
		refused bool
	}{
		{name: "empty"},
		{name: "ordinary splice", kept: 20, added: []int{4, 2}, want: 26},
		{name: "maximum unchanged", kept: math.MaxInt, want: math.MaxInt},
		{name: "exact fit", kept: math.MaxInt - 3, added: []int{1, 2}, want: math.MaxInt},
		{name: "one byte too large", kept: math.MaxInt, added: []int{1}, refused: true},
		{name: "later addition overflows", kept: math.MaxInt - 2, added: []int{2, 1}, refused: true},
		{name: "multiple large additions", kept: 1, added: []int{math.MaxInt, math.MaxInt}, refused: true},
		{name: "negative kept size", kept: -1, refused: true},
		{name: "negative added size", kept: 1, added: []int{-1}, refused: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkedSpliceSize(tt.kept, tt.added...)
			if (err != nil) != tt.refused {
				t.Fatalf("size=%d error=%v; refusal=%t", got, err, tt.refused)
			}
			if err == nil && got != tt.want {
				t.Fatalf("size=%d; want %d", got, tt.want)
			}
		})
	}
}
