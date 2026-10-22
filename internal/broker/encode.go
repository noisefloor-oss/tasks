package broker

import (
	"strings"
	"unicode/utf8"
)

// appendJSONString writes one minimal-escaping JSON string. Path and name
// fields are validated to a printable subset first (ValidateBindings), so
// escaping stays byte-stable for HMAC purposes.
func appendJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := range len(s) {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 || c == 0x7f {
				// Encoded as a short unicode escape so the canonical form
				// never depends on raw control bytes.
				const hexdigits = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hexdigits[c>>4])
				b.WriteByte(hexdigits[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
}

// takeJSONString consumes one canonical JSON string at s[p:] using the same
// minimal escaping, returning the decoded value and the new offset.
func takeJSONString(s string, p int) (string, int, error) {
	if p >= len(s) || s[p] != '"' {
		return "", p, refuse("capability encoding is not canonical")
	}
	p++
	var b strings.Builder
	for p < len(s) {
		c := s[p]
		switch {
		case c == '"':
			return b.String(), p + 1, nil
		case c == '\\':
			if p+1 >= len(s) {
				return "", p, refuse("capability encoding is not canonical")
			}
			switch s[p+1] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'u':
				if p+5 >= len(s) || s[p+2] != '0' || s[p+3] != '0' {
					return "", p, refuse("capability encoding is not canonical")
				}
				v := hexVal(s[p+4])
				w := hexVal(s[p+5])
				if v < 0 || w < 0 {
					return "", p, refuse("capability encoding is not canonical")
				}
				b.WriteByte(byte(v<<4 | w))
				p += 4
			default:
				return "", p, refuse("capability encoding is not canonical")
			}
			p += 2
		case c < 0x20 || c == 0x7f:
			return "", p, refuse("capability encoding is not canonical")
		default:
			b.WriteByte(c)
			p++
		}
	}
	return "", p, refuse("capability encoding is not canonical")
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return -1
	}
}

// Sanitize reduces arbitrary bytes (for example captured Git output) to one
// bounded, single-line, printable reason. No path beyond the already-bound
// values, no environment, and no credential survives sanitization.
func Sanitize(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == ' ':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
	out := strings.Join(strings.Fields(s), " ")
	if !utf8.ValidString(out) {
		out = ""
	}
	if len(out) > MaxReason {
		out = out[:MaxReason]
		// Avoid splitting a rune.
		for !utf8.ValidString(out) && len(out) > 0 {
			out = out[:len(out)-1]
		}
	}
	return out
}
