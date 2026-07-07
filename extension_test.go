package kamt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ported from ref-fvm ipld/kamt/src/ext.rs

func TestExtensionLongest(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 0b10001000
	key[1] = 0b10010010
	key[2] = 0b10101010
	key[3] = 0b11011011
	key[4] = 0b11101110

	key2 := append([]byte(nil), key...)
	key2[3] = 0b11010111
	key3 := append([]byte(nil), key...)
	key3[4] = 0b11111110

	hb := newHashBits(key)
	bitWidth := 3
	// consume some of the key
	n, err := hb.next(bitWidth * 2)
	require.NoError(t, err)
	require.Equal(t, 0b00100010, n)

	// the common prefix should run from here to somewhere inside key[3]
	ext, err := longestCommonPrefix(hb, bitWidth, [][]byte{key2, key3})
	require.NoError(t, err)
	// the first 4 bits of key[3] match, but we take bitWidth at a time, which
	// stops at the 3rd bit
	require.Equal(t, 2+8+8+3, ext.length)
	require.Len(t, ext.path, 3)
	require.Equal(t, byte(0b00100100), ext.path[0])
	require.Equal(t, byte(0b10101010), ext.path[1])
	require.Equal(t, byte(0b10110000), ext.path[2])
	totalConsumed := 2*bitWidth + ext.length
	require.Equal(t, totalConsumed, hb.consumed)

	hb = newHashBitsAt(key, 2*bitWidth)
	matched, err := ext.longestMatch(hb, bitWidth)
	require.NoError(t, err)
	require.Equal(t, ext.length, matched)
	require.Equal(t, totalConsumed, hb.consumed)
	// shouldn't work a second time
	matched, err = ext.longestMatch(hb, bitWidth)
	require.NoError(t, err)
	require.Equal(t, 0, matched)
	require.Equal(t, totalConsumed, hb.consumed)
}

func TestExtensionSplit(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 0b10001000
	key[1] = 0b10010010
	key[2] = 0b10101010
	key[3] = 0b11011011
	key[4] = 0b11101110

	bitWidth := 3
	hb := newHashBits(key)
	_, err := hb.next(bitWidth)
	require.NoError(t, err)

	ext, err := extensionFromBits(hb, 253)
	require.NoError(t, err)
	require.Equal(t, 253, ext.length)
	require.Equal(t, byte(0b01000100), ext.path[0])

	head, midx, tail, err := ext.split(20, bitWidth)
	require.NoError(t, err)

	require.Equal(t, 20, head.length)
	require.Equal(t, byte(0b01000100), head.path[0])
	require.Equal(t, byte(0b10010101), head.path[1])
	require.Equal(t, byte(0b01010000), head.path[2])

	require.Equal(t, 3, midx.length)
	require.Equal(t, byte(0b01100000), midx.path[0])

	require.Equal(t, 230, tail.length)
	require.Equal(t, byte(0b01101111), tail.path[0])
	require.Equal(t, byte(0b10111000), tail.path[1])

	ext2, err := unsplitExtensions(&head, &midx, &tail)
	require.NoError(t, err)
	require.Equal(t, ext, ext2)
}
