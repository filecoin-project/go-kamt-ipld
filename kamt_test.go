package kamt

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	block "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/stretchr/testify/require"
)

type mockBlocks struct {
	data   map[cid.Cid]block.Block
	dataMu sync.Mutex
}

func newMockBlocks() *mockBlocks {
	return &mockBlocks{data: make(map[cid.Cid]block.Block)}
}

func (mb *mockBlocks) Get(_ context.Context, c cid.Cid) (block.Block, error) {
	mb.dataMu.Lock()
	defer mb.dataMu.Unlock()
	if d, ok := mb.data[c]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("not found")
}

func (mb *mockBlocks) Put(_ context.Context, b block.Block) error {
	mb.dataMu.Lock()
	defer mb.dataMu.Unlock()
	mb.data[b.Cid()] = b
	return nil
}

// key32 makes a 32-byte big-endian key from an integer, the shape the EVM
// actor uses for storage slots. Consecutive integers share long prefixes,
// exercising extensions heavily.
func key32(i uint64) []byte {
	k := make([]byte, 32)
	binary.BigEndian.PutUint64(k[24:], i)
	return k
}

func randKey32(r *rand.Rand) []byte {
	k := make([]byte, 32)
	r.Read(k)
	return k
}

func value(i uint64) *cbg.CborInt {
	v := cbg.CborInt(i)
	return &v
}

var testConfigs = map[string][]Option{
	"defaults":     nil,
	"evm":          {UseTreeBitWidth(5), UseMaxArrayWidth(1)},
	"bitwidth-1":   {UseTreeBitWidth(1)},
	"bw4-mindata1": {UseTreeBitWidth(4), UseMinDataDepth(1)},
	"bw2-bucket1":  {UseTreeBitWidth(2), UseMaxArrayWidth(1)},
}

func forEachConfig(t *testing.T, f func(t *testing.T, options []Option)) {
	for name, options := range testConfigs {
		t.Run(name, func(t *testing.T) { f(t, options) })
	}
}

func TestSetGetDelete(t *testing.T) {
	forEachConfig(t, func(t *testing.T, options []Option) {
		ctx := context.Background()
		cs := cbor.NewCborStore(newMockBlocks())
		n, err := NewNode(cs, options...)
		require.NoError(t, err)

		const count = 200
		for i := uint64(0); i < count; i++ {
			require.NoError(t, n.Set(ctx, key32(i), value(i)))
		}
		for i := uint64(0); i < count; i++ {
			var out cbg.CborInt
			found, err := n.Find(ctx, key32(i), &out)
			require.NoError(t, err)
			require.True(t, found, "key %d", i)
			require.Equal(t, cbg.CborInt(i), out)
		}
		found, err := n.Find(ctx, key32(count+1), nil)
		require.NoError(t, err)
		require.False(t, found)

		// persist, reload, and read everything back through the store
		c, err := n.Write(ctx)
		require.NoError(t, err)
		n2, err := LoadNode(ctx, cs, c, options...)
		require.NoError(t, err)

		seen := 0
		var prev []byte
		err = n2.ForEach(ctx, func(k []byte, val *cbg.Deferred) error {
			require.Len(t, k, 32)
			if prev != nil {
				require.Negative(t, bytes.Compare(prev, k), "keys must arrive in order")
			}
			prev = append(prev[:0], k...)
			seen++
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, count, seen)

		// delete everything and end with an empty root
		for i := uint64(0); i < count; i++ {
			deleted, err := n2.Delete(ctx, key32(i))
			require.NoError(t, err)
			require.True(t, deleted, "key %d", i)
		}
		deleted, err := n2.Delete(ctx, key32(0))
		require.NoError(t, err)
		require.False(t, deleted)

		c2, err := n2.Write(ctx)
		require.NoError(t, err)
		empty, err := NewNode(cs, options...)
		require.NoError(t, err)
		cEmpty, err := empty.Write(ctx)
		require.NoError(t, err)
		require.Equal(t, cEmpty, c2, "empty-again tree must equal the empty tree")
	})
}

func TestInsertOrderIndependence(t *testing.T) {
	forEachConfig(t, func(t *testing.T, options []Option) {
		ctx := context.Background()
		cs := cbor.NewCborStore(newMockBlocks())
		r := rand.New(rand.NewSource(42))

		keys := make([][]byte, 100)
		for i := range keys {
			if i%2 == 0 {
				keys[i] = key32(uint64(i)) // clustered
			} else {
				keys[i] = randKey32(r) // scattered
			}
		}

		build := func(order []int) cid.Cid {
			n, err := NewNode(cs, options...)
			require.NoError(t, err)
			for _, i := range order {
				require.NoError(t, n.Set(ctx, keys[i], value(uint64(i))))
			}
			c, err := n.Write(ctx)
			require.NoError(t, err)
			return c
		}

		order := make([]int, len(keys))
		for i := range order {
			order[i] = i
		}
		expected := build(order)
		for trial := 0; trial < 5; trial++ {
			r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			require.Equal(t, expected, build(order), "insert order must not affect the root CID")
		}
	})
}

// TestDeleteRestoresCanonicalForm walks a growing tree, snapshotting the
// root CID at each size, then deletes back down verifying every snapshot is
// reproduced exactly. This exercises collapse and extension unsplitting: any
// deviation from canonical form after a delete produces a different CID.
func TestDeleteRestoresCanonicalForm(t *testing.T) {
	forEachConfig(t, func(t *testing.T, options []Option) {
		ctx := context.Background()
		cs := cbor.NewCborStore(newMockBlocks())
		r := rand.New(rand.NewSource(101))

		const count = 100
		keys := make([][]byte, count)
		for i := range keys {
			if i%3 == 0 {
				keys[i] = randKey32(r)
			} else {
				keys[i] = key32(uint64(i * 3))
			}
		}

		n, err := NewNode(cs, options...)
		require.NoError(t, err)
		snapshots := make([]cid.Cid, count+1)
		c, err := n.Write(ctx)
		require.NoError(t, err)
		snapshots[0] = c
		for i, k := range keys {
			require.NoError(t, n.Set(ctx, k, value(uint64(i))))
			c, err := n.Write(ctx)
			require.NoError(t, err)
			snapshots[i+1] = c
		}

		for i := count - 1; i >= 0; i-- {
			deleted, err := n.Delete(ctx, keys[i])
			require.NoError(t, err)
			require.True(t, deleted)
			c, err := n.Write(ctx)
			require.NoError(t, err)
			require.Equal(t, snapshots[i], c, "delete of key %d must restore snapshot %d", i, i)
		}
	})
}

// TestEvolvedEqualsFresh applies a random churn of sets, overwrites and
// deletes, then verifies the evolved tree has the same CID as one built
// fresh from only the surviving entries.
func TestEvolvedEqualsFresh(t *testing.T) {
	forEachConfig(t, func(t *testing.T, options []Option) {
		ctx := context.Background()
		cs := cbor.NewCborStore(newMockBlocks())
		r := rand.New(rand.NewSource(7))

		n, err := NewNode(cs, options...)
		require.NoError(t, err)

		model := make(map[string]uint64)
		keyspace := make([][]byte, 60)
		for i := range keyspace {
			if i%2 == 0 {
				keyspace[i] = key32(uint64(i))
			} else {
				keyspace[i] = randKey32(r)
			}
		}

		for op := 0; op < 1000; op++ {
			k := keyspace[r.Intn(len(keyspace))]
			if r.Intn(3) == 0 {
				_, err := n.Delete(ctx, k)
				require.NoError(t, err)
				delete(model, string(k))
			} else {
				v := r.Uint64() % 1000
				require.NoError(t, n.Set(ctx, k, value(v)))
				model[string(k)] = v
			}
		}

		evolved, err := n.Write(ctx)
		require.NoError(t, err)

		fresh, err := NewNode(cs, options...)
		require.NoError(t, err)
		for k, v := range model {
			require.NoError(t, fresh.Set(ctx, []byte(k), value(v)))
		}
		freshCid, err := fresh.Write(ctx)
		require.NoError(t, err)

		require.Equal(t, freshCid, evolved, "churned tree must equal a fresh build of its content")

		// and the content must match the model exactly
		seen := make(map[string]uint64)
		require.NoError(t, n.ForEach(ctx, func(k []byte, val *cbg.Deferred) error {
			var out cbg.CborInt
			if err := out.UnmarshalCBOR(bytes.NewReader(val.Raw)); err != nil {
				return err
			}
			seen[string(k)] = uint64(out)
			return nil
		}))
		require.Equal(t, model, seen)
	})
}

func TestSetIfAbsent(t *testing.T) {
	ctx := context.Background()
	cs := cbor.NewCborStore(newMockBlocks())
	n, err := NewNode(cs, UseTreeBitWidth(5), UseMaxArrayWidth(1))
	require.NoError(t, err)

	set, err := n.SetIfAbsent(ctx, key32(1), value(1))
	require.NoError(t, err)
	require.True(t, set)
	set, err = n.SetIfAbsent(ctx, key32(1), value(2))
	require.NoError(t, err)
	require.False(t, set)

	var out cbg.CborInt
	found, err := n.Find(ctx, key32(1), &out)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, cbg.CborInt(1), out)
}

func TestKeyLengthEnforced(t *testing.T) {
	ctx := context.Background()
	cs := cbor.NewCborStore(newMockBlocks())
	n, err := NewNode(cs)
	require.NoError(t, err)

	require.Error(t, n.Set(ctx, []byte{1, 2, 3}, value(1)))
	_, err = n.Find(ctx, []byte{1, 2, 3}, nil)
	require.Error(t, err)
	_, err = n.Delete(ctx, make([]byte, 33))
	require.Error(t, err)
}

func TestPointerExtensionAccessor(t *testing.T) {
	ctx := context.Background()
	cs := cbor.NewCborStore(newMockBlocks())
	n, err := NewNode(cs, UseTreeBitWidth(5), UseMaxArrayWidth(1))
	require.NoError(t, err)

	// Keys 0 and 1 differ only in the final bit of 256. The root consumes
	// bits 0-4; the shared bits 5-254 (250 bits, 50 levels) become a single
	// link extension down to the level where the keys diverge.
	require.NoError(t, n.Set(ctx, key32(0), value(0)))
	require.NoError(t, n.Set(ctx, key32(1), value(1)))

	root, err := n.Write(ctx)
	require.NoError(t, err)
	n, err = LoadNode(ctx, cs, root, UseTreeBitWidth(5), UseMaxArrayWidth(1))
	require.NoError(t, err)

	require.Len(t, n.Pointers, 1)
	lengthBits, path := n.Pointers[0].Extension()
	require.Equal(t, 250, lengthBits)
	require.Equal(t, make([]byte, 32), path) // ceil(250/8), all-zero prefix

	// The returned path is a copy; mutating it must not touch the pointer.
	path[0] = 0xff
	_, again := n.Pointers[0].Extension()
	require.Equal(t, make([]byte, 32), again)

	// The divergent level holds two buckets; bucket pointers have no
	// extension.
	child, err := n.Pointers[0].loadChild(ctx, cs, n.cfg, 1)
	require.NoError(t, err)
	require.Len(t, child.Pointers, 2)
	for _, p := range child.Pointers {
		lengthBits, path := p.Extension()
		require.Zero(t, lengthBits)
		require.Nil(t, path)
	}
}

func TestForEachNode(t *testing.T) {
	ctx := context.Background()
	cs := cbor.NewCborStore(newMockBlocks())
	n, err := NewNode(cs, UseTreeBitWidth(5), UseMaxArrayWidth(1))
	require.NoError(t, err)

	const count = 500
	for i := uint64(0); i < count; i++ {
		require.NoError(t, n.Set(ctx, key32(i*7), value(i)))
	}
	root, err := n.Write(ctx)
	require.NoError(t, err)
	n, err = LoadNode(ctx, cs, root, UseTreeBitWidth(5), UseMaxArrayWidth(1))
	require.NoError(t, err)

	var forEachKeys [][]byte
	require.NoError(t, n.ForEach(ctx, func(k []byte, _ *cbg.Deferred) error {
		forEachKeys = append(forEachKeys, k)
		return nil
	}))

	var nodeWalkKeys [][]byte
	nodes := 0
	require.NoError(t, n.ForEachNode(ctx, func(info NodeInfo) error {
		if nodes == 0 {
			require.False(t, info.CID.Defined()) // root reached by no link
			require.Zero(t, info.Depth)
			require.Zero(t, info.LogicalDepth)
		} else {
			require.True(t, info.CID.Defined())
			require.Positive(t, info.Depth)
		}
		require.GreaterOrEqual(t, info.LogicalDepth, info.Depth)
		nodes++
		for _, p := range info.Node.Pointers {
			lengthBits, _ := p.Extension()
			require.Zero(t, lengthBits%5)
			for _, kv := range p.KVs {
				nodeWalkKeys = append(nodeWalkKeys, kv.Key)
			}
		}
		return nil
	}))

	// Every entry is reachable exactly once through node buckets. Order may
	// differ from ForEach: a node's buckets are visited before its child
	// nodes regardless of pointer interleaving.
	require.Greater(t, nodes, 1)
	require.ElementsMatch(t, forEachKeys, nodeWalkKeys)
}
