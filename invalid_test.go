package kamt

import (
	"bytes"
	"context"
	"testing"

	block "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	mh "github.com/multiformats/go-multihash"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/stretchr/testify/require"
)

// Malformed and non-canonical block rejection. Blocks are hand-assembled
// CBOR so that shapes our own marshalers refuse to produce can be tested.

// raw CBOR construction helpers

func cborHeader(maj byte, val uint64) []byte {
	switch {
	case val < 24:
		return []byte{maj<<5 | byte(val)}
	case val < 1<<8:
		return []byte{maj<<5 | 24, byte(val)}
	case val < 1<<16:
		return []byte{maj<<5 | 25, byte(val >> 8), byte(val)}
	default:
		panic("larger than test helpers need")
	}
}

func cborBytes(b []byte) []byte {
	return append(cborHeader(2, uint64(len(b))), b...)
}

func cborText(s string) []byte {
	return append(cborHeader(3, uint64(len(s))), s...)
}

func cborArray(items ...[]byte) []byte {
	out := cborHeader(4, uint64(len(items)))
	for _, item := range items {
		out = append(out, item...)
	}
	return out
}

func cborMap1(key string, val []byte) []byte {
	out := cborHeader(5, 1)
	out = append(out, cborText(key)...)
	return append(out, val...)
}

func cborCid(c cid.Cid) []byte {
	out := []byte{0xd8, 0x2a} // tag 42
	return append(out, cborBytes(append([]byte{0}, c.Bytes()...))...)
}

// structure helpers

func rawNode(bitfield []byte, pointers ...[]byte) []byte {
	return cborArray(cborBytes(bitfield), cborArray(pointers...))
}

func rawBucket(kvs ...[]byte) []byte {
	return cborMap1("v", cborArray(kvs...))
}

func rawKV(key []byte, value []byte) []byte {
	return cborArray(cborBytes(key), value)
}

func rawLink(c cid.Cid, extLen int, extPath []byte) []byte {
	return cborMap1("l", cborArray(cborCid(c), cborHeader(0, uint64(extLen)), cborBytes(extPath)))
}

// putRaw stores raw block bytes under their DAG-CBOR/Blake2b-256 CID.
func putRaw(t *testing.T, blocks *mockBlocks, data []byte) cid.Cid {
	t.Helper()
	pref := cid.Prefix{Version: 1, Codec: cid.DagCBOR, MhType: mh.BLAKE2B_MIN + 31, MhLength: 32}
	c, err := pref.Sum(data)
	require.NoError(t, err)
	blk, err := block.NewBlockWithCid(data, c)
	require.NoError(t, err)
	require.NoError(t, blocks.Put(context.Background(), blk))
	return c
}

func loadRaw(t *testing.T, data []byte, depth int, options ...Option) error {
	t.Helper()
	blocks := newMockBlocks()
	c := putRaw(t, blocks, data)
	cfg := defaultConfig()
	for _, option := range options {
		require.NoError(t, option(cfg))
	}
	_, err := loadNode(context.Background(), cbor.NewCborStore(blocks), cfg, c, depth)
	return err
}

var evmOptions = []Option{UseTreeBitWidth(5), UseMaxArrayWidth(1)}

func TestLoadValidBaseline(t *testing.T) {
	// sanity-check the helpers: a well-formed single-bucket node loads
	data := rawNode([]byte{0b1}, rawBucket(rawKV([]byte{1}, cborHeader(0, 42))))
	require.NoError(t, loadRaw(t, data, 0, evmOptions...))
}

func TestLoadRejectsPointerCountMismatch(t *testing.T) {
	bucket := rawBucket(rawKV([]byte{1}, cborHeader(0, 1)))
	data := rawNode([]byte{0b1}, bucket, bucket)
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "doesn't match bitfield")
}

func TestLoadRejectsTooManyPointers(t *testing.T) {
	bucket := rawBucket(rawKV([]byte{1}, cborHeader(0, 1)))
	data := rawNode([]byte{0b111}, bucket, bucket, bucket)
	require.ErrorContains(t, loadRaw(t, data, 0, UseTreeBitWidth(1)), "exceeds")
}

func TestLoadRejectsBitfieldOutOfRange(t *testing.T) {
	// bit 32 set with bitWidth 5 (slots 0..31)
	data := rawNode([]byte{0x01, 0, 0, 0, 0}, rawBucket(rawKV([]byte{1}, cborHeader(0, 1))))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "beyond")
}

func TestLoadRejectsNonMinimalBitfield(t *testing.T) {
	data := rawNode([]byte{0x00, 0x01}, rawBucket(rawKV([]byte{1}, cborHeader(0, 1))))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "leading zero")
}

func TestLoadRejectsNonRootEmptyNode(t *testing.T) {
	data := rawNode([]byte{})
	require.NoError(t, loadRaw(t, data, 0, evmOptions...), "empty root is the one legal empty node")
	require.ErrorIs(t, loadRaw(t, data, 1, evmOptions...), errZeroPointers)
}

func TestLoadRejectsEmptyBucket(t *testing.T) {
	data := rawNode([]byte{0b1}, rawBucket())
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "empty bucket")
}

func TestLoadRejectsOversizeBucket(t *testing.T) {
	data := rawNode([]byte{0b1}, rawBucket(
		rawKV([]byte{1}, cborHeader(0, 1)),
		rawKV([]byte{2}, cborHeader(0, 2)),
	))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "too many items")
}

func TestLoadRejectsUnsortedBucket(t *testing.T) {
	data := rawNode([]byte{0b1}, rawBucket(
		rawKV([]byte{2}, cborHeader(0, 1)),
		rawKV([]byte{1}, cborHeader(0, 2)),
	))
	require.ErrorContains(t, loadRaw(t, data, 0, UseTreeBitWidth(5)), "unsorted")
}

func TestLoadRejectsDuplicateKeys(t *testing.T) {
	data := rawNode([]byte{0b1}, rawBucket(
		rawKV([]byte{1}, cborHeader(0, 1)),
		rawKV([]byte{1}, cborHeader(0, 2)),
	))
	require.ErrorContains(t, loadRaw(t, data, 0, UseTreeBitWidth(5)), "duplicate")
}

func TestLoadRejectsPaddedKey(t *testing.T) {
	// zeroless form is canonical; the Rust reader accepts this shape but
	// can never produce it
	data := rawNode([]byte{0b1}, rawBucket(rawKV([]byte{0x00, 0x01}, cborHeader(0, 1))))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "leading zero")
}

func TestLoadRejectsOversizeKey(t *testing.T) {
	key := make([]byte, 33)
	key[0] = 1
	data := rawNode([]byte{0b1}, rawBucket(rawKV(key, cborHeader(0, 1))))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "exceeds the key length")
}

func TestLoadRejectsValuesAboveMinDataDepth(t *testing.T) {
	data := rawNode([]byte{0b1}, rawBucket(rawKV([]byte{1}, cborHeader(0, 1))))
	require.ErrorContains(t, loadRaw(t, data, 0, UseTreeBitWidth(4), UseMinDataDepth(1)),
		"minimum data depth")
}

func testCid(t *testing.T, codec uint64) cid.Cid {
	t.Helper()
	pref := cid.Prefix{Version: 1, Codec: codec, MhType: mh.BLAKE2B_MIN + 31, MhLength: 32}
	c, err := pref.Sum([]byte("test"))
	require.NoError(t, err)
	return c
}

func TestLoadRejectsBadLinkCodec(t *testing.T) {
	data := rawNode([]byte{0b1}, rawLink(testCid(t, cid.Raw), 5, []byte{0xf8}))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "DAG-CBOR")
}

func TestLoadRejectsExtLengthNotMultipleOfBitWidth(t *testing.T) {
	data := rawNode([]byte{0b1}, rawLink(testCid(t, cid.DagCBOR), 7, []byte{0xf8}))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "multiple")
}

func TestLoadRejectsExtTooLong(t *testing.T) {
	// at depth 0 with bitWidth 5 and 32-byte keys, 251 bits remain below;
	// an extension must be strictly shorter
	path := make([]byte, 32)
	for i := range path {
		path[i] = 0xff
	}
	data := rawNode([]byte{0b1}, rawLink(testCid(t, cid.DagCBOR), 255, path[:32]))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "less than")
}

func TestLoadRejectsExtTrailingBits(t *testing.T) {
	// 5-bit extension leaves 3 trailing bits in its byte; they must be zero.
	// The Rust reader accepts this shape but can never produce it.
	data := rawNode([]byte{0b1}, rawLink(testCid(t, cid.DagCBOR), 5, []byte{0b00001001}))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "trailing bits")
}

func TestLoadRejectsExtPathLengthMismatch(t *testing.T) {
	// 20 bits requires 3 path bytes, only 1 given; rejected at decode
	data := rawNode([]byte{0b1}, rawLink(testCid(t, cid.DagCBOR), 20, []byte{0xff}))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "expected")
}

func TestLoadRejectsUnknownPointerVariant(t *testing.T) {
	data := rawNode([]byte{0b1}, cborMap1("x", cborArray()))
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "unknown pointer variant")
}

func TestLoadRejectsMultiEntryPointerMap(t *testing.T) {
	ptr := cborHeader(5, 2) // map(2)
	ptr = append(ptr, cborText("v")...)
	ptr = append(ptr, cborArray(rawKV([]byte{1}, cborHeader(0, 1)))...)
	ptr = append(ptr, cborText("l")...)
	ptr = append(ptr, cborArray()...)
	data := rawNode([]byte{0b1}, ptr)
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "single-entry map")
}

func TestLoadRejectsTrailingNodeBytes(t *testing.T) {
	data := rawNode([]byte{0b1}, rawBucket(rawKV([]byte{1}, cborHeader(0, 42))))
	data = append(data, 0xf6) // CBOR null after a complete node
	require.ErrorContains(t, loadRaw(t, data, 0, evmOptions...), "trailing bytes")
}

func TestLoadRejectsBadRootCodec(t *testing.T) {
	data := rawNode([]byte{})
	blocks := newMockBlocks()
	pref := cid.Prefix{Version: 1, Codec: cid.Raw, MhType: mh.BLAKE2B_MIN + 31, MhLength: 32}
	c, err := pref.Sum(data)
	require.NoError(t, err)
	blk, err := block.NewBlockWithCid(data, c)
	require.NoError(t, err)
	require.NoError(t, blocks.Put(context.Background(), blk))
	_, err = LoadNode(context.Background(), cbor.NewCborStore(blocks), c, evmOptions...)
	require.ErrorContains(t, err, "root must be DAG-CBOR")
}

func TestLoadRejectsUncollapsedNodes(t *testing.T) {
	// a non-root node with a single pointer should have been spliced into
	// its parent, whether the pointer is a bucket or a link
	single := rawNode([]byte{0b1}, rawBucket(rawKV([]byte{1}, cborHeader(0, 1))))
	require.ErrorContains(t, loadRaw(t, single, 1, evmOptions...), "collapsed into its parent")
	singleLink := rawNode([]byte{0b1}, rawLink(testCid(t, cid.DagCBOR), 5, []byte{0xf8}))
	require.ErrorContains(t, loadRaw(t, singleLink, 1, evmOptions...), "collapsed into its parent")

	// a non-root node whose buckets would fit within one bucket should have
	// been collapsed (default maxArrayWidth 3, two buckets of one entry)
	collapsible := rawNode([]byte{0b11},
		rawBucket(rawKV([]byte{1}, cborHeader(0, 1))),
		rawBucket(rawKV([]byte{0x21}, cborHeader(0, 2))),
	)
	require.ErrorContains(t, loadRaw(t, collapsible, 1, UseTreeBitWidth(5)),
		"collapsed into a single parent bucket")

	// with maxArrayWidth 1 the same shape is canonical
	require.NoError(t, loadRaw(t, collapsible, 1, evmOptions...))

	// above minDataDepth the collapse rules are suspended: a single-bucket
	// node at depth 1 under minDataDepth 1 is exactly what inserts produce
	require.NoError(t, loadRaw(t, single, 1, UseTreeBitWidth(4), UseMinDataDepth(1)))

	// the root is exempt: a single-bucket root is a one-entry tree
	require.NoError(t, loadRaw(t, single, 0, evmOptions...))
}

func TestMaxArrayWidthBounds(t *testing.T) {
	ctx := context.Background()
	cs := cbor.NewCborStore(newMockBlocks())

	_, err := NewNode(cs, UseMaxArrayWidth(maxDecodeKVs+1))
	require.ErrorContains(t, err, "maxArrayWidth must be between")

	// the maximum permitted width round-trips through the decoder
	n, err := NewNode(cs, UseTreeBitWidth(8), UseMaxArrayWidth(maxDecodeKVs))
	require.NoError(t, err)
	for i := uint64(0); i < maxDecodeKVs; i++ {
		// keys sharing a first slot so they occupy one bucket
		k := key32(i)
		k[0] = 0x80
		require.NoError(t, n.Set(ctx, k, value(i)))
	}
	c, err := n.Write(ctx)
	require.NoError(t, err)
	n2, err := LoadNode(ctx, cs, c, UseTreeBitWidth(8), UseMaxArrayWidth(maxDecodeKVs))
	require.NoError(t, err)
	count := 0
	require.NoError(t, n2.ForEach(ctx, func([]byte, *cbg.Deferred) error { count++; return nil }))
	require.Equal(t, maxDecodeKVs, count)
}

// TestDeleteReportsDeletionOnCleanupError builds, in memory, the locally
// valid but non-minimal shape root -> link -> single-bucket child (which
// load validation now rejects on the wire). Deleting the only key empties
// the child; cleanup fails, but the deletion must still be reported so the
// caller knows the tree was modified.
func TestDeleteReportsDeletionOnCleanupError(t *testing.T) {
	ctx := context.Background()
	cs := cbor.NewCborStore(newMockBlocks())
	n, err := NewNode(cs, UseTreeBitWidth(5), UseMaxArrayWidth(1))
	require.NoError(t, err)

	k := key32(0)
	child := &Node{store: cs, cfg: n.cfg}
	child.insertPointer(0, &Pointer{KVs: []*KV{{Key: k, Value: &cbg.Deferred{Raw: []byte{0x01}}}}})
	n.insertPointer(0, &Pointer{cache: child, dirty: true})

	deleted, err := n.Delete(ctx, k)
	require.ErrorIs(t, err, errZeroPointers)
	require.True(t, deleted, "the entry was removed before cleanup failed")
}

func TestMarshalRejectsImpossibleStates(t *testing.T) {
	var buf bytes.Buffer

	// pointer that is both link and bucket
	p := &Pointer{
		Link: testCid(t, cid.DagCBOR),
		KVs:  []*KV{{Key: []byte{1}, Value: &cbg.Deferred{Raw: []byte{0x01}}}},
	}
	require.ErrorContains(t, p.MarshalCBOR(&buf), "both a link and a bucket")

	// pointer that is neither
	require.ErrorContains(t, (&Pointer{}).MarshalCBOR(&buf), "link or a non-empty bucket")

	// node whose bitfield disagrees with its pointer count
	n := &Node{}
	n.Bitfield.setBit(0)
	n.Bitfield.setBit(1)
	n.Pointers = []*Pointer{{KVs: []*KV{{Key: []byte{1}, Value: &cbg.Deferred{Raw: []byte{0x01}}}}}}
	require.ErrorContains(t, n.MarshalCBOR(&buf), "bits set")
}

func TestSetRawRejectsMalformedValues(t *testing.T) {
	ctx := context.Background()
	n, err := NewNode(cbor.NewCborStore(newMockBlocks()), evmOptions...)
	require.NoError(t, err)

	// truncated CBOR item
	require.ErrorContains(t, n.SetRaw(ctx, key32(1), []byte{0x82, 0x01}), "well-formed")
	// trailing bytes after a complete item
	require.ErrorContains(t, n.SetRaw(ctx, key32(1), []byte{0x01, 0x01}), "trailing")
}
