package kamt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ported from ref-fvm ipld/kamt/src/hash_bits.rs

func TestHashBitsChomping(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 0b10001000
	key[1] = 0b10101010
	key[2] = 0b10111111
	key[3] = 0b11111111

	hb := newHashBits(key)

	// consume exactly the rest of the current byte
	n, err := hb.next(8)
	require.NoError(t, err)
	require.Equal(t, 0b10001000, n)
	// consume less than the rest of the current byte
	n, err = hb.next(5)
	require.NoError(t, err)
	require.Equal(t, 0b10101, n)
	// consume across a byte boundary
	n, err = hb.next(5)
	require.NoError(t, err)
	require.Equal(t, 0b01010, n)
	n, err = hb.next(6)
	require.NoError(t, err)
	require.Equal(t, 0b111111, n)
	n, err = hb.next(8)
	require.NoError(t, err)
	require.Equal(t, 0b11111111, n)

	_, err = hb.next(9)
	require.ErrorIs(t, err, errInvalidBitLen)

	// iterate through the rest of the key
	for i := 0; i < 28; i++ {
		_, err = hb.next(8)
		require.NoError(t, err)
	}
	_, err = hb.next(1)
	require.ErrorIs(t, err, ErrMaxDepth)
}

func TestHashBitsPartialLastBits(t *testing.T) {
	key := make([]byte, 32)
	key[31] = 0b00000001
	bitWidth := 5

	hb := newHashBits(key)
	for i := 0; i < 256/bitWidth; i++ {
		_, err := hb.next(bitWidth)
		require.NoError(t, err)
	}
	// 255 bits consumed, 1 remains; a bitWidth read returns just that bit
	n, err := hb.next(bitWidth)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	_, err = hb.next(bitWidth)
	require.ErrorIs(t, err, ErrMaxDepth)
}
