// Package canonical implements replay-core's canonical-digest law:
// byte-exact canonical JSON for the kernel and commons profiles, and
// SHA-256 digests over the encoded bytes. The conformance corpus under
// testdata/corpus is the normative form of the law.
package canonical

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
)

// Kind tags the JSON value a Value holds. Int and Float are distinct
// kinds: the law renders them differently and the commons profile
// forbids floats outright, so a number's kind must survive decoding.
type Kind int

const (
	KindNull Kind = iota
	KindBool
	KindString
	KindInt
	KindFloat
	KindArray
	KindObject
)

// Pair is one object member. Object order in a Value is insertion
// order; the encoder sorts by key, so construction order never
// affects the canonical bytes.
type Pair struct {
	Key   string
	Value Value
}

// Value is a JSON value that preserves integer precision exactly:
// an Int keeps its decimal text (arbitrary precision), never a
// float64 approximation.
type Value struct {
	Kind  Kind
	Bool  bool
	Str   string
	Int   string // canonical decimal text, set when Kind == KindInt
	Float float64
	Elems []Value
	Pairs []Pair
}

func Null() Value         { return Value{Kind: KindNull} }
func BoolOf(b bool) Value { return Value{Kind: KindBool, Bool: b} }
func StringOf(s string) Value {
	return Value{Kind: KindString, Str: s}
}
func FloatOf(f float64) Value { return Value{Kind: KindFloat, Float: f} }

func ArrayOf(elems ...Value) Value { return Value{Kind: KindArray, Elems: elems} }
func ObjectOf(pairs ...Pair) Value { return Value{Kind: KindObject, Pairs: pairs} }

// IntOf builds an Int from decimal text, normalized to canonical
// form (no leading zeros, "-0" becomes "0") at arbitrary precision.
func IntOf(text string) (Value, error) {
	bi, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return Value{}, fmt.Errorf("invalid integer text %q", text)
	}
	return Value{Kind: KindInt, Int: bi.String()}, nil
}

// FromJSON decodes JSON into a Value using json.Number so integer
// text is preserved exactly. A number literal with a fraction or
// exponent is a Float; any other number literal is an Int. Duplicate
// object keys are rejected: the law's inputs are objects, and a
// duplicate key has no canonical meaning.
func FromJSON(data []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return Value{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return Value{}, fmt.Errorf("trailing data after JSON value")
		}
		return Value{}, err
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return Value{}, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := Value{Kind: KindObject}
			seen := make(map[string]bool)
			for dec.More() {
				ktok, err := dec.Token()
				if err != nil {
					return Value{}, err
				}
				key, ok := ktok.(string)
				if !ok {
					return Value{}, fmt.Errorf("object key is not a string")
				}
				if seen[key] {
					return Value{}, fmt.Errorf("duplicate canonical object key %q", key)
				}
				seen[key] = true
				elem, err := decodeValue(dec)
				if err != nil {
					return Value{}, err
				}
				v.Pairs = append(v.Pairs, Pair{Key: key, Value: elem})
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return Value{}, err
			}
			return v, nil
		case '[':
			v := Value{Kind: KindArray}
			for dec.More() {
				elem, err := decodeValue(dec)
				if err != nil {
					return Value{}, err
				}
				v.Elems = append(v.Elems, elem)
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return Value{}, err
			}
			return v, nil
		}
		return Value{}, fmt.Errorf("unexpected delimiter %q", string(t))
	case string:
		return StringOf(t), nil
	case bool:
		return BoolOf(t), nil
	case nil:
		return Null(), nil
	case json.Number:
		return numberValue(string(t))
	}
	return Value{}, fmt.Errorf("unexpected token %T", tok)
}

// numberValue classifies a JSON number literal: fraction or exponent
// means Float, anything else is an exact Int.
func numberValue(text string) (Value, error) {
	if strings.ContainsAny(text, ".eE") {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return Value{}, fmt.Errorf("invalid float literal %q: %w", text, err)
		}
		return FloatOf(f), nil
	}
	return IntOf(text)
}
