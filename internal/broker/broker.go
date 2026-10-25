// Package broker implements the §9.5 confined worktree-registration broker.
//
// The broker is stateless and SQLite-blind. Its only inputs are one verb,
// an authenticated host capability inherited on fd 3, and the installed
// capability key. It validates every binding, drops permanently to the
// invoking uid/gid before any Git or target effect, runs Git with a fixed
// argv and a stripped environment, and reports only bounded success or a
// sanitized refusal.
package broker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Verbs are the exact §9.5 effecting verbs. Nothing else is accepted.
const (
	VerbCreate            = "create"
	VerbRemove            = "remove"
	VerbPruneRegistration = "prune-registration"
)

// CapabilityVersion is the sole accepted capability schema version.
const CapabilityVersion = 1

// capabilityDomain separates broker capability MACs from every other use of
// the installed key.
const capabilityDomain = "noise-worktree-broker-capability-v1\x00"

// Limits keep every accepted value bounded before any parsing side effect.
const (
	maxFamilyID    = 128
	maxSagaFence   = 128
	maxPathBytes   = 4096
	maxBranchByte  = 200
	maxCommitBytes = 64
	maxCapability  = 64 * 1024
	// MaxReason bounds every sanitized refusal reason.
	MaxReason = 200
)

// ErrRefused marks every rejection that must surface as a bounded,
// sanitized refusal (exit 1). Usage errors are reported separately.
var ErrRefused = errors.New("broker refusal")

// RefusalError carries one bounded, sanitized refusal reason.
type RefusalError struct{ Reason string }

func (e *RefusalError) Error() string { return e.Reason }

// refuse builds a RefusalError from a format and already-trusted arguments.
func refuse(format string, args ...any) error {
	return &RefusalError{Reason: Sanitize(fmt.Sprintf(format, args...))}
}

// Capability is the §9.5 host capability. Every field is bound: verb,
// family id, saga fence, registration root (the common directory's
// worktrees directory), owner boundary, exact target and branch, immutable
// saga-recorded base commit, force intent, invoking uid/gid, and expiry. It
// carries no claim/run token and no credential.
type Capability struct {
	Verb             string
	FamilyID         string
	SagaFence        string
	RegistrationRoot string
	OwnerBoundary    string
	Target           string
	Branch           string
	// BaseCommit is the immutable saga-recorded base commit. Empty is the
	// §9.5 legacy-cleanup null, valid only with prune-registration.
	BaseCommit string
	Force      bool
	UID        int
	GID        int
	Expires    int64
}

// Key is the shared host/broker HMAC secret.
type Key []byte

// LoadKey validates key material: 32 bytes from a root-only file.
func LoadKey(b []byte) (Key, error) {
	if len(b) != 32 {
		return nil, refuse("capability key must be 32 bytes")
	}
	return Key(b), nil
}

// Mint renders the authenticated capability document for cap: one line of
// canonical JSON and one line of hex HMAC-SHA256.
func Mint(key Key, cap Capability) ([]byte, error) {
	body, err := canonicalCapability(cap)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(capabilityDomain))
	mac.Write(body)
	doc := string(body) + "\n" + hex.EncodeToString(mac.Sum(nil)) + "\n"
	return []byte(doc), nil
}

// Parse verifies and decodes one minted capability document.
func Parse(key Key, doc []byte, now int64) (*Capability, error) {
	if len(doc) > maxCapability {
		return nil, refuse("capability too large")
	}
	s := string(doc)
	s = strings.TrimSuffix(s, "\n")
	i := strings.LastIndexByte(s, '\n')
	if i < 0 {
		return nil, refuse("capability has no authenticator")
	}
	body, tag := s[:i], s[i+1:]
	if len(body) == 0 || tag == "" {
		return nil, refuse("capability is truncated")
	}
	sum, err := hex.DecodeString(tag)
	if err != nil || len(sum) != sha256.Size {
		return nil, refuse("capability authenticator is malformed")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(capabilityDomain))
	mac.Write([]byte(body))
	if !hmac.Equal(mac.Sum(nil), sum) {
		return nil, refuse("capability authentication failed")
	}
	cap, err := decodeCanonical(body)
	if err != nil {
		return nil, err
	}
	if cap.Expires <= now {
		return nil, refuse("capability has expired")
	}
	return cap, nil
}

// canonicalCapability renders the strict canonical encoding: fixed field
// order, no whitespace, JSON string escaping for path and name fields.
func canonicalCapability(cap Capability) ([]byte, error) {
	var b strings.Builder
	b.WriteString(`{"version":`)
	b.WriteString(strconv.Itoa(CapabilityVersion))
	b.WriteString(`,"verb":`)
	appendJSONString(&b, cap.Verb)
	b.WriteString(`,"family_id":`)
	appendJSONString(&b, cap.FamilyID)
	b.WriteString(`,"saga_fence":`)
	appendJSONString(&b, cap.SagaFence)
	b.WriteString(`,"registration_root":`)
	appendJSONString(&b, cap.RegistrationRoot)
	b.WriteString(`,"owner_boundary":`)
	appendJSONString(&b, cap.OwnerBoundary)
	b.WriteString(`,"target":`)
	appendJSONString(&b, cap.Target)
	b.WriteString(`,"branch":`)
	appendJSONString(&b, cap.Branch)
	b.WriteString(`,"base_commit":`)
	appendJSONString(&b, cap.BaseCommit)
	b.WriteString(`,"force":`)
	b.WriteString(strconv.FormatBool(cap.Force))
	b.WriteString(`,"uid":`)
	b.WriteString(strconv.Itoa(cap.UID))
	b.WriteString(`,"gid":`)
	b.WriteString(strconv.Itoa(cap.GID))
	b.WriteString(`,"expires":`)
	b.WriteString(strconv.FormatInt(cap.Expires, 10))
	b.WriteString("}")
	return []byte(b.String()), nil
}

// decodeCanonical parses the exact canonical encoding; any deviation,
// duplicate field, unknown field, or trailing bytes are a refusal.
func decodeCanonical(body string) (*Capability, error) {
	// Field order is fixed by Mint, so a strict positional decode is the
	// complete grammar. Each step consumes its literal prefix.
	var cap Capability
	p := 0
	take := func(lit string) error {
		if !strings.HasPrefix(body[p:], lit) {
			return refuse("capability encoding is not canonical")
		}
		p += len(lit)
		return nil
	}
	var err error
	if err = take(`{"version":`); err != nil {
		return nil, err
	}
	ver, r := parseInt(body[p:])
	if ver != CapabilityVersion {
		return nil, refuse("capability version is unsupported")
	}
	p += r
	fields := []struct {
		lit  string
		dest *string
	}{
		{`,"verb":`, &cap.Verb},
		{`,"family_id":`, &cap.FamilyID},
		{`,"saga_fence":`, &cap.SagaFence},
		{`,"registration_root":`, &cap.RegistrationRoot},
		{`,"owner_boundary":`, &cap.OwnerBoundary},
		{`,"target":`, &cap.Target},
		{`,"branch":`, &cap.Branch},
		{`,"base_commit":`, &cap.BaseCommit},
	}
	for _, f := range fields {
		if err = take(f.lit); err != nil {
			return nil, err
		}
		*(*string)(f.dest), p, err = takeJSONString(body, p)
		if err != nil {
			return nil, err
		}
	}
	if err = take(`,"force":`); err != nil {
		return nil, err
	}
	if strings.HasPrefix(body[p:], "true") {
		cap.Force, p = true, p+4
	} else if strings.HasPrefix(body[p:], "false") {
		cap.Force, p = false, p+5
	} else {
		return nil, refuse("capability encoding is not canonical")
	}
	if err = take(`,"uid":`); err != nil {
		return nil, err
	}
	cap.UID, p, err = parseIntAt(body, p)
	if err != nil {
		return nil, err
	}
	if err = take(`,"gid":`); err != nil {
		return nil, err
	}
	cap.GID, p, err = parseIntAt(body, p)
	if err != nil {
		return nil, err
	}
	if err = take(`,"expires":`); err != nil {
		return nil, err
	}
	cap.Expires, p, err = parseInt64At(body, p)
	if err != nil {
		return nil, err
	}
	if err = take("}"); err != nil {
		return nil, err
	}
	if p != len(body) {
		return nil, refuse("capability has trailing data")
	}
	canonical, err := canonicalCapability(cap)
	if err != nil || string(canonical) != body {
		return nil, refuse("capability encoding is not canonical")
	}
	return &cap, nil
}

func parseInt(s string) (int, int) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || (i > 1 && s[0] == '0') {
		return -1, i
	}
	v, err := strconv.ParseUint(s[:i], 10, strconv.IntSize)
	if err != nil {
		return -1, i
	}
	return int(v), i
}

func parseIntAt(s string, p int) (int, int, error) {
	v, n := parseInt(s[p:])
	if n == 0 || v < 0 {
		return 0, p, refuse("capability encoding is not canonical")
	}
	return v, p + n, nil
}

func parseInt64At(s string, p int) (int64, int, error) {
	n := 0
	for p+n < len(s) && s[p+n] >= '0' && s[p+n] <= '9' {
		n++
	}
	if n == 0 || (n > 1 && s[p] == '0') {
		return 0, p, refuse("capability encoding is not canonical")
	}
	v, err := strconv.ParseInt(s[p:p+n], 10, 64)
	if err != nil {
		return 0, p, refuse("capability encoding is not canonical")
	}
	return v, p + n, nil
}
