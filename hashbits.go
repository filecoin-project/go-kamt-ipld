package kamt

// hashBits is a helper for reading a key as a sequence of small integers,
// `bitWidth` bits at a time, from the most significant bit of the first byte
// onward. State is retained: each call to next() consumes more of the key.
//
// Unlike the HAMT equivalent this does not wrap a hash digest; KAMT keys are
// used directly. It can also wrap a partial key (an extension path) whose
// usable length is not a whole number of bytes.
type hashBits struct {
	b        []byte
	length   int // usable length in bits, may be less than len(b)*8
	consumed int
}

// newHashBits wraps a complete key.
func newHashBits(b []byte) *hashBits {
	return &hashBits{b: b, length: len(b) * 8}
}

// newHashBitsAt wraps a complete key with some bits already consumed.
func newHashBitsAt(b []byte, consumed int) *hashBits {
	return &hashBits{b: b, length: len(b) * 8, consumed: consumed}
}

// newHashBitsSlice wraps a partial key of the given length in bits, as found
// in an extension path.
func newHashBitsSlice(b []byte, lengthBits int) *hashBits {
	return &hashBits{b: b, length: lengthBits}
}

func mkmask(n int) byte {
	return (1 << uint(n)) - 1
}

// next returns the next 'i' bits of the key as an integer. At the end of the
// key fewer than 'i' bits may remain; the remainder is returned (e.g. a
// 256-bit key consumed 5 bits at a time leaves 1 final bit). Returns
// ErrMaxDepth when the key is exhausted.
func (hb *hashBits) next(i int) (int, error) {
	if i > 8 || i == 0 {
		return 0, errInvalidBitLen
	}
	if hb.consumed >= hb.length {
		return 0, ErrMaxDepth
	}
	if rem := hb.length - hb.consumed; i > rem {
		i = rem
	}
	return hb.nextBits(i), nil
}

func (hb *hashBits) nextBits(i int) int {
	curbi := hb.consumed / 8
	leftb := 8 - (hb.consumed % 8)

	curb := hb.b[curbi]
	switch {
	case i == leftb:
		// bits to consume equal the bits remaining in the current byte
		out := int(mkmask(i) & curb)
		hb.consumed += i
		return out
	case i < leftb:
		// consuming less than the remaining bits in the current byte
		a := curb & mkmask(leftb)    // mask out the high bits we don't want
		b := a & ^mkmask(leftb-i)    // mask out the low bits we don't want
		c := int(b >> uint(leftb-i)) // shift what's left down
		hb.consumed += i
		return c
	default:
		// consume the remaining bits of this byte and recurse into the next
		out := int(mkmask(leftb) & curb)
		out <<= uint(i - leftb)
		hb.consumed += leftb
		out += hb.nextBits(i - leftb)
		return out
	}
}
