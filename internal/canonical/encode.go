package canonical

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// Profile prefixes: every canonical encoding begins with the
// profile's version tag line, and the digest covers it.
const (
	kernelPrefix  = "replaycore.kernel.canonical.v1\n"
	commonsPrefix = "commons.canonical.v1\n"
)

type profile int

const (
	profileKernel profile = iota
	profileCommons
)

// FloatForbiddenError reports a float under the commons profile,
// which forbids floats in canonical inputs. Path names the JSON
// location of the offending value (e.g. "$.a[0].x").
type FloatForbiddenError struct{ Path string }

func (e *FloatForbiddenError) Error() string {
	return "floats are forbidden in canonical inputs at " + e.Path +
		"; encode as strings or use schema-declared integer pairs"
}

// NonFiniteError reports a NaN or infinite float, which the law
// rejects under both profiles.
type NonFiniteError struct{ Path string }

func (e *NonFiniteError) Error() string {
	return "non-finite floats are forbidden in canonical inputs at " + e.Path
}

// MarshalKernel encodes v under the kernel profile: raw UTF-8,
// keys sorted by Unicode code point, compact separators.
func MarshalKernel(v Value) ([]byte, error) { return marshal(v, profileKernel) }

// MarshalCommons encodes v under the commons profile: non-ASCII
// escaped as lowercase \uXXXX, floats forbidden.
func MarshalCommons(v Value) ([]byte, error) { return marshal(v, profileCommons) }

func marshal(v Value, p profile) ([]byte, error) {
	var buf bytes.Buffer
	if p == profileKernel {
		buf.WriteString(kernelPrefix)
	} else {
		buf.WriteString(commonsPrefix)
	}
	if err := writeValue(&buf, v, p, "$"); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeValue(buf *bytes.Buffer, v Value, p profile, path string) error {
	switch v.Kind {
	case KindNull:
		buf.WriteString("null")
	case KindBool:
		if v.Bool {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case KindString:
		writeString(buf, v.Str, p)
	case KindInt:
		// Int text is canonical decimal, written verbatim so
		// arbitrary-precision integers never pass through float64.
		buf.WriteString(v.Int)
	case KindFloat:
		if math.IsNaN(v.Float) || math.IsInf(v.Float, 0) {
			return &NonFiniteError{Path: path}
		}
		if p == profileCommons {
			return &FloatForbiddenError{Path: path}
		}
		// Python-repr rendering (number.go). Non-finite floats were
		// rejected above, so FormatFloat's own error is a backstop.
		fs, err := FormatFloat(v.Float)
		if err != nil {
			return err
		}
		buf.WriteString(fs)
	case KindArray:
		buf.WriteByte('[')
		for i, e := range v.Elems {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeValue(buf, e, p, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case KindObject:
		// The law sorts keys by Unicode code point; sorting is on a
		// copy so the caller's Value is never mutated.
		pairs := make([]Pair, len(v.Pairs))
		copy(pairs, v.Pairs)
		sort.SliceStable(pairs, func(i, j int) bool {
			return compareCodePoints(pairs[i].Key, pairs[j].Key) < 0
		})
		buf.WriteByte('{')
		for i, pair := range pairs {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, pair.Key, p)
			buf.WriteByte(':')
			if err := writeValue(buf, pair.Value, p, path+"."+pair.Key); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unknown value kind %d at %s", int(v.Kind), path)
	}
	return nil
}

// compareCodePoints compares two keys by Unicode code point, the
// law's sort order (UTF-16 unit order would rank astral characters
// differently; decoding to runes makes the code-point order exact).
func compareCodePoints(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if ra[i] != rb[i] {
			if ra[i] < rb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(ra) < len(rb):
		return -1
	case len(ra) > len(rb):
		return 1
	}
	return 0
}

// writeString writes a JSON string literal under the profile's
// escaping law: `"` and `\` always escaped; \n \r \t \b \f use the
// short forms; other control characters below 0x20 use \u00XX with
// lowercase hex. The kernel profile emits everything else as raw
// UTF-8. The commons profile additionally escapes every rune at or
// above 0x7F as \uXXXX (lowercase), using a UTF-16 surrogate pair
// for astral runes.
func writeString(buf *bytes.Buffer, s string, p profile) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Not valid UTF-8: keep the raw byte under the kernel
			// profile; the commons profile's ASCII law substitutes
			// the replacement character's escape.
			if p == profileCommons {
				buf.WriteString(`\ufffd`)
			} else {
				buf.WriteByte(s[i])
			}
			i++
			continue
		}
		i += size
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(buf, `\u%04x`, r)
			case p == profileCommons && r >= 0x7f:
				if r > 0xFFFF {
					hi, lo := utf16.EncodeRune(r)
					fmt.Fprintf(buf, `\u%04x\u%04x`, hi, lo)
				} else {
					fmt.Fprintf(buf, `\u%04x`, r)
				}
			default:
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}
