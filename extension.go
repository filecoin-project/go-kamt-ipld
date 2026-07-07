package kamt

// extension is the portion of a key path that a link skips over. Because
// KAMT keys are not hashed, sets of keys with long common prefixes are
// expected (e.g. consecutive Solidity slots); without extensions those
// prefixes would materialize as long chains of single-pointer intermediate
// nodes. A link carries the skipped path so that readers can match keys
// against it and writers can split it when a diverging key arrives.
type extension struct {
	// length in bits of the extension between the node containing the link
	// and the node the link points to. May be less than len(path)*8; trailing
	// bits of the final byte are zero.
	length int
	// the part of the key covered by the extension, packed MSB-first
	path []byte
}

func (e *extension) isEmpty() bool {
	return e.length == 0
}

func (e *extension) pathBits() *hashBits {
	return newHashBitsSlice(e.path, e.length)
}

// longestMatch consumes up to bitWidth bits at a time from the key,
// comparing against this extension's path, and returns the total number of
// bits matched. Consumption stops (and the key's cursor rewinds one step) at
// the first mismatch.
func (e *extension) longestMatch(hk *hashBits, bitWidth int) (int, error) {
	path := e.pathBits()
	matched := 0
	for matched < e.length {
		consumed := hk.consumed
		n1, err := hk.next(bitWidth)
		if err != nil {
			return 0, err
		}
		n2, err := path.next(bitWidth)
		if err != nil {
			return 0, err
		}
		if n1 != n2 {
			hk.consumed = consumed
			break
		}
		matched += bitWidth
	}
	return matched, nil
}

// longestCommonPrefix finds the longest prefix shared between the key (from
// its current cursor position) and all of the given keys, consuming bitWidth
// bits at a time, and returns it as an extension. The matched bits are
// consumed from hk.
func longestCommonPrefix(hk *hashBits, bitWidth int, keys [][]byte) (extension, error) {
	others := make([]*hashBits, len(keys))
	for i, k := range keys {
		others[i] = newHashBitsAt(k, hk.consumed)
	}

	var builder extensionBuilder
	totalBits := hk.length

consume:
	for hk.consumed < totalBits {
		consumed := hk.consumed
		n, err := hk.next(bitWidth)
		if err != nil {
			return extension{}, err
		}

		for _, o := range others {
			no, err := o.next(bitWidth)
			if err != nil {
				return extension{}, err
			}
			if n != no {
				hk.consumed = consumed
				break consume
			}
		}

		builder.add(bitWidth, byte(n))
	}

	return builder.build(), nil
}

// split divides the extension after `consumed` bits into a head, the
// bitWidth-sized index that follows it, and the tail beyond that.
func (e *extension) split(consumed, bitWidth int) (head, idx, tail extension, err error) {
	path := e.pathBits()
	if head, err = extensionFromBits(path, consumed); err != nil {
		return
	}
	if idx, err = extensionFromBits(path, bitWidth); err != nil {
		return
	}
	tail, err = extensionFromBits(path, e.length-head.length-idx.length)
	return
}

// unsplitExtensions merges head+idx+tail back into a single extension,
// undoing a prior split.
func unsplitExtensions(ext1, idx, ext2 *extension) (extension, error) {
	return mergeExtensions(idx.length, ext1, idx, ext2)
}

func mergeExtensions(bitWidth int, exts ...*extension) (extension, error) {
	var builder extensionBuilder
	for _, ext := range exts {
		path := ext.pathBits()
		bitsLeft := ext.length
		for bitsLeft > 0 {
			i := min(bitWidth, bitsLeft)
			n, err := path.next(i)
			if err != nil {
				return extension{}, err
			}
			builder.add(i, byte(n))
			bitsLeft -= i
		}
	}
	return builder.build(), nil
}

// extensionFromBits builds an extension from the next `length` bits of the
// given cursor.
func extensionFromBits(bits *hashBits, length int) (extension, error) {
	var builder extensionBuilder
	for length > 0 {
		i := min(length, 8)
		n, err := bits.next(i)
		if err != nil {
			return extension{}, err
		}
		length -= i
		builder.add(i, byte(n))
	}
	return builder.build(), nil
}

// extensionFromIdx builds a single-level extension from a node index.
func extensionFromIdx(idx, bitWidth int) extension {
	var builder extensionBuilder
	builder.add(bitWidth, byte(idx))
	return builder.build()
}

// extensionBuilder packs bit groups into bytes, MSB-first.
type extensionBuilder struct {
	written int
	out     byte
	path    []byte
}

func (b *extensionBuilder) add(bitWidth int, n byte) {
	// how far we have filled the current byte
	j := b.written % 8
	i := bitWidth
	if j+i > 8 {
		// The next bits don't fit in the current byte. Take the leftmost bits,
		// append the full byte to the path, then start a new one and write the
		// rightmost bits into that.
		carry := j + i - 8
		b.out += n >> uint(carry)
		b.path = append(b.path, b.out)
		b.out = n & mkmask(carry)
		b.out <<= uint(8 - carry)
	} else {
		// The previous byte isn't full yet; shift the bits into alignment and
		// fill the next leftmost positions.
		b.out += n << uint(8-j-i)
	}
	b.written += i

	if b.written%8 == 0 {
		b.path = append(b.path, b.out)
		b.out = 0
	}
}

func (b *extensionBuilder) build() extension {
	path := b.path
	if b.written%8 != 0 {
		path = append(path, b.out)
	}
	return extension{length: b.written, path: path}
}
