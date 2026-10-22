package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"
)

// Canonical JSON (SPEC §8.5): object keys sorted by raw UTF-8 bytes, array
// order preserved, minimal string escapes, string bytes preserved without
// Unicode normalization, integers in minimal base 10, and only strings,
// integers, booleans, null, arrays, and objects admitted. This rendering is
// the byte format the versioned semantic and comparison digests cover.

// canonicalJSON renders v in §8.5 canonical JSON. v must be composed of nil,
// bool, string, json.Number, int/int64, []any, and map[string]any — the
// shapes produced by decodeJSON and by the digest record builders.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch value := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if value {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, value)
	case json.Number:
		return writeCanonicalInteger(buf, value.String())
	case int:
		fmt.Fprintf(buf, "%d", value)
	case int64:
		fmt.Fprintf(buf, "%d", value)
	case []any:
		buf.WriteByte('[')
		for i, elem := range value {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Strings(keys) // byte-wise UTF-8 order
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, value[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonical JSON: unsupported value of type %T", v)
	}
	return nil
}

// writeCanonicalString emits s with minimal escapes: only the mandatory
// control-character, quote, and backslash escapes; every other byte passes
// through unaltered (no Unicode normalization, no HTML or U+2028 escaping).
func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"', '\\':
			buf.WriteByte('\\')
			buf.WriteByte(c)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, c)
			} else {
				buf.WriteByte(c)
			}
		}
	}
	buf.WriteByte('"')
}

// writeCanonicalInteger renders a JSON integer literal in minimal base 10.
// Non-integer numbers are outside the §8.5 value domain and fail closed.
func writeCanonicalInteger(buf *bytes.Buffer, literal string) error {
	if !jsonIntegerLiteral(literal) {
		return fmt.Errorf("canonical JSON: non-integer number %q", literal)
	}
	n, ok := new(big.Int).SetString(literal, 10)
	if !ok {
		return fmt.Errorf("canonical JSON: invalid integer %q", literal)
	}
	buf.WriteString(n.String())
	return nil
}

func jsonIntegerLiteral(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// decodeJSON parses data into the canonical value domain (objects as
// map[string]any with json.Number leaves), rejecting duplicate object keys at
// every level and any trailing content. This is the strict bounded parser the
// §8.5 manifest and digest record decoding share.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing content")
	}
	return v, nil
}

// rejectDuplicateKeys walks raw JSON tokens and reports the first duplicate
// object key at any nesting level, plus trailing content. It extends the
// strict parser family to byte-faithful raw documents (the §8.6 legacy
// surface) whose unknown fields must be preserved verbatim, so it validates
// without re-rendering the values.
func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := scanDuplicateKeys(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("unexpected trailing content")
	}
	return nil
}

func scanDuplicateKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar leaf
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = true
			if err := scanDuplicateKeys(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := scanDuplicateKeys(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return fmt.Errorf("unexpected delimiter %q", delim)
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				if _, dup := obj[key]; dup {
					return nil, fmt.Errorf("duplicate object key %q", key)
				}
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			var arr []any
			for dec.More() {
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			if arr == nil {
				arr = []any{}
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %q", t)
	case json.Number:
		if !jsonIntegerLiteral(t.String()) {
			return nil, fmt.Errorf("non-integer number %s is outside the canonical domain", t)
		}
		return t, nil
	case string, bool, nil:
		return t, nil
	case float64:
		return nil, fmt.Errorf("non-integer number is outside the canonical domain")
	default:
		return nil, fmt.Errorf("unexpected token %v", tok)
	}
}

// decodeJSONObject parses data and requires the top level to be an object.
func decodeJSONObject(data []byte) (map[string]any, error) {
	v, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("top level is not an object")
	}
	return obj, nil
}

// decodeJSONArray parses data and requires the top level to be an array.
func decodeJSONArray(data []byte) ([]any, error) {
	v, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("top level is not an array")
	}
	return arr, nil
}

// canonicalizeJSON re-renders raw JSON in canonical form.
func canonicalizeJSON(raw []byte) ([]byte, error) {
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(v)
}

// canonicalObjectString renders a small string-keyed record, a convenience
// for digest framing of fixed shapes.
func canonicalObjectString(fields map[string]any) (string, error) {
	raw, err := canonicalJSON(fields)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
