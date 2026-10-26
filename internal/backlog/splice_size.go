package backlog

import (
	"fmt"
	"math"
)

func checkedSpliceSize(kept int, added ...int) (int, error) {
	if kept < 0 {
		return 0, fmt.Errorf("invalid JSON splice size")
	}
	size := kept
	for _, n := range added {
		if n < 0 || n > math.MaxInt-size {
			return 0, fmt.Errorf("JSON splice exceeds maximum buffer size")
		}
		size += n
	}
	return size, nil
}
