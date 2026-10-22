// Package envcompat provides bounded environment-variable compatibility for
// the noise-tasks transition. It deliberately fails closed when old and new
// names disagree instead of silently selecting one configuration.
package envcompat

import (
	"fmt"
	"os"
)

// Lookup returns the non-empty value configured under canonical or legacy.
// Equal values are accepted when both names are present. Empty values behave
// as unset, matching os.Getenv-based configuration used before the rename.
func Lookup(canonical, legacy string) (string, error) {
	canonicalValue := os.Getenv(canonical)
	legacyValue := os.Getenv(legacy)
	if canonicalValue != "" && legacyValue != "" && canonicalValue != legacyValue {
		return "", fmt.Errorf("conflicting environment variables %s and %s", canonical, legacy)
	}
	if canonicalValue != "" {
		return canonicalValue, nil
	}
	return legacyValue, nil
}
