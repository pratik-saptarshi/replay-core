package canonical

import (
	"bytes"
	"fmt"
)

// The strict decoders are the inverse of the Marshal functions, with
// the law's byte-exactness as the acceptance rule: the input must be
// exactly what Marshal would emit for the value it encodes, profile
// prefix included. Parsing alone is not enough — JSON that merely
// denotes the same value (unsorted keys, extra whitespace, escaped
// forms the profile would emit raw, non-canonical number literals)
// is rejected by the byte-equality check, so "these bytes are
// canonical" is a property a caller can rely on, not re-derive.

// DecodeKernel decodes bytes that must be in kernel-profile
// canonical form.
func DecodeKernel(data []byte) (Value, error) { return decode(data, profileKernel) }

// DecodeCommons decodes bytes that must be in commons-profile
// canonical form.
func DecodeCommons(data []byte) (Value, error) { return decode(data, profileCommons) }

func decode(data []byte, p profile) (Value, error) {
	prefix, name := kernelPrefix, "kernel"
	if p == profileCommons {
		prefix, name = commonsPrefix, "commons"
	}
	if !bytes.HasPrefix(data, []byte(prefix)) {
		return Value{}, fmt.Errorf("canonical bytes: missing %s profile prefix", name)
	}
	v, err := FromJSON(data[len(prefix):])
	if err != nil {
		return Value{}, err
	}
	re, err := marshal(v, p)
	if err != nil {
		return Value{}, err
	}
	if !bytes.Equal(re, data) {
		return Value{}, fmt.Errorf("canonical bytes: input is not in canonical byte form for the %s profile", name)
	}
	return v, nil
}
