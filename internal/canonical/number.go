package canonical

import (
	"math"
	"strconv"
	"strings"
)

// FormatFloat renders f byte-identically to Python's repr(float) as
// emitted by json.dumps — the kernel profile's float law:
//
//   - shortest round-trip decimal digits;
//   - fixed notation iff -4 <= exp < 16, where exp is the decimal
//     exponent of the value in scientific form (d.ddd x 10^exp);
//   - integral values in fixed notation get a trailing ".0";
//   - exponent notation is d[.ddd]e±XX: one digit before the point,
//     the exponent always signed and zero-padded to at least two
//     digits, and no ".0" when there are no fractional digits
//     (1e21 renders "1e+21", not "1.0e+21").
//
// NaN and ±Inf are rejected: the law forbids non-finite floats.
func FormatFloat(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", &NonFiniteError{Path: "$"}
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0", nil
		}
		return "0.0", nil
	}

	neg := math.Signbit(f)
	// Extract the shortest round-trip digits and the decimal exponent
	// from Go's scientific form ("d.ddde±XX"); the digit sequence is
	// the same one Python's repr computes, only the rendering differs.
	s := strconv.FormatFloat(math.Abs(f), 'e', -1, 64)
	epos := strings.IndexByte(s, 'e')
	exp, err := strconv.Atoi(s[epos+1:])
	if err != nil {
		return "", err // unreachable: FormatFloat 'e' output is well-formed
	}
	digits := strings.Replace(s[:epos], ".", "", 1)

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	if exp < -4 || exp >= 16 {
		// Exponent notation.
		b.WriteByte(digits[0])
		if len(digits) > 1 {
			b.WriteByte('.')
			b.WriteString(digits[1:])
		}
		b.WriteByte('e')
		if exp < 0 {
			b.WriteByte('-')
		} else {
			b.WriteByte('+')
		}
		es := strconv.Itoa(abs(exp))
		if len(es) < 2 {
			es = "0" + es
		}
		b.WriteString(es)
		return b.String(), nil
	}
	// Fixed notation: the decimal point falls pointPos digits into
	// the digit string.
	pointPos := exp + 1
	switch {
	case pointPos <= 0:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -pointPos))
		b.WriteString(digits)
	case pointPos >= len(digits):
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", pointPos-len(digits)))
		b.WriteString(".0")
	default:
		b.WriteString(digits[:pointPos])
		b.WriteByte('.')
		b.WriteString(digits[pointPos:])
	}
	return b.String(), nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
