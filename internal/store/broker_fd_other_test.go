//go:build !unix

package store

import "testing"

func withBrokerCapabilityFD(t *testing.T, _ string, _ func()) {
	t.Helper()
	t.Skip("fd-3 broker capability forwarding requires Unix")
}

func withBrokerCapabilityFDPath(t *testing.T, _ string, _ func(string)) {
	t.Helper()
	t.Skip("fd-3 broker capability forwarding requires Unix")
}

func withoutBrokerCapabilityFD(t *testing.T, _ func()) {
	t.Helper()
	t.Skip("fd-3 broker capability forwarding requires Unix")
}
