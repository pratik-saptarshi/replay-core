package canonical

import (
	"crypto/sha256"
	"encoding/hex"
)

// DigestKernel returns the kernel-profile digest of v:
// "sha256:<lowercase hex>" of its canonical bytes, prefix included.
func DigestKernel(v Value) (string, error) {
	b, err := MarshalKernel(v)
	if err != nil {
		return "", err
	}
	return digestBytes(b), nil
}

// DigestCommons returns the commons-profile digest of v.
func DigestCommons(v Value) (string, error) {
	b, err := MarshalCommons(v)
	if err != nil {
		return "", err
	}
	return digestBytes(b), nil
}

func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
