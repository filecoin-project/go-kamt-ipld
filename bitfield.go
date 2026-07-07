package kamt

import (
	"fmt"
	"io"
	"math/big"
	"math/bits"

	cbg "github.com/whyrusleeping/cbor-gen"
)

// Bitfield records which of a node's 2^bitWidth slots are occupied. It is
// serialized as a minimal (no leading zeros) big-endian byte string, the same
// wire form the Rust implementation produces from its fixed [u64; 4].
type Bitfield struct {
	big.Int
}

func (b *Bitfield) MarshalCBOR(w io.Writer) error {
	return cbg.WriteByteArray(w, b.Bytes())
}

func (b *Bitfield) UnmarshalCBOR(r io.Reader) error {
	// 32 bytes covers the 256 slots of the maximum bitWidth of 8
	byts, err := cbg.ReadByteArray(r, 32)
	if err != nil {
		return err
	}
	// minimal encoding is part of canonical form
	if len(byts) > 0 && byts[0] == 0 {
		return fmt.Errorf("bitfield encoding has leading zero bytes")
	}
	b.SetBytes(byts)
	return nil
}

func (b *Bitfield) testBit(i int) bool {
	return b.Bit(i) == 1
}

func (b *Bitfield) setBit(i int) {
	b.SetBit(&b.Int, i, 1)
}

func (b *Bitfield) clearBit(i int) {
	b.SetBit(&b.Int, i, 0)
}

func (b *Bitfield) countOnes() int {
	count := 0
	for _, w := range b.Bits() {
		count += bits.OnesCount(uint(w))
	}
	return count
}

// lastOneIdx returns the position of the highest set bit, or -1 if none are
// set.
func (b *Bitfield) lastOneIdx() int {
	return b.BitLen() - 1
}

// indexForBitPos returns the index within the compacted Pointers array
// corresponding to the given bit in the bitfield: a popcount of the bits
// below `bp`. e.g. a bitfield of 10010110000 has 4 pointers; bit position 7
// maps to Pointers[2] because two lower bits are set.
func indexForBitPos(bp int, bitfield *big.Int) int {
	var x uint
	var count, i int
	w := bitfield.Bits()
	for x = uint(bp); x > bits.UintSize && i < len(w); x -= bits.UintSize {
		count += bits.OnesCount(uint(w[i]))
		i++
	}
	if i == len(w) {
		return count
	}
	return count + bits.OnesCount(uint(w[i])&((1<<x)-1))
}
