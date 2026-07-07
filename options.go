package kamt

import "fmt"

const (
	defaultBitWidth      = 8
	defaultMinDataDepth  = 0
	defaultMaxArrayWidth = 3
	defaultKeyLength     = 32
)

type config struct {
	// bitWidth determines how many bits of the key are consumed at each
	// level, giving nodes 2^bitWidth slots.
	bitWidth int
	// minDataDepth is the shallowest depth at which key-value pairs may be
	// stored; levels above it hold only links. Keeps frequently read upper
	// nodes small.
	minDataDepth int
	// maxArrayWidth is the number of key-value pairs a bucket may hold
	// before it is pushed down into a new child node.
	maxArrayWidth int
	// keyLength is the fixed length, in bytes, of every key.
	keyLength int
}

func defaultConfig() *config {
	return &config{
		bitWidth:      defaultBitWidth,
		minDataDepth:  defaultMinDataDepth,
		maxArrayWidth: defaultMaxArrayWidth,
		keyLength:     defaultKeyLength,
	}
}

// Option is a configuration parameter for a KAMT instance. The configuration
// is not stored in the serialized form; every reader and writer of a given
// structure must supply identical parameters or the data will be unreadable,
// or worse, produce divergent CIDs on write.
type Option func(*config) error

// UseTreeBitWidth sets the number of bits of the key consumed at each level
// of the tree, from 1 to 8, giving nodes 2^bitWidth slots. The EVM actor's
// contract storage uses 5.
func UseTreeBitWidth(bitWidth int) Option {
	return func(c *config) error {
		if bitWidth < 1 || bitWidth > 8 {
			return fmt.Errorf("bitWidth must be between 1 and 8, got %d", bitWidth)
		}
		c.bitWidth = bitWidth
		return nil
	}
}

// UseMinDataDepth reserves the levels above the given depth for links only;
// no key-value pairs will be stored in them. The default of 0 allows data in
// the root node.
func UseMinDataDepth(depth int) Option {
	return func(c *config) error {
		if depth < 0 {
			return fmt.Errorf("minDataDepth must not be negative, got %d", depth)
		}
		c.minDataDepth = depth
		return nil
	}
}

// UseMaxArrayWidth sets the number of key-value pairs a bucket may hold
// before it is pushed down into a new child node, up to 1024 so that every
// writable tree round-trips through the decoder. The EVM actor's contract
// storage uses 1.
func UseMaxArrayWidth(width int) Option {
	return func(c *config) error {
		if width < 1 || width > maxDecodeKVs {
			return fmt.Errorf("maxArrayWidth must be between 1 and %d, got %d", maxDecodeKVs, width)
		}
		c.maxArrayWidth = width
		return nil
	}
}

// UseKeyLength sets the fixed key length in bytes. Every key supplied to
// this KAMT must be exactly this long. The EVM actor's contract storage uses
// 32 (big-endian U256).
func UseKeyLength(length int) Option {
	return func(c *config) error {
		if length < 1 || length > 64 {
			return fmt.Errorf("keyLength must be between 1 and 64, got %d", length)
		}
		c.keyLength = length
		return nil
	}
}
