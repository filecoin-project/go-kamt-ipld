package kamt

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	block "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/stretchr/testify/require"
)

// Differential fixtures generated from the reference Rust implementation
// (fvm_ipld_kamt) by fixtures/generator. Each records a deterministic
// operation sequence with the resulting root CID and every reachable block.
// Replaying the operations must produce byte-identical blocks and the same
// root.

type fixture struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Config struct {
		BitWidth      int `json:"bitWidth"`
		MinDataDepth  int `json:"minDataDepth"`
		MaxArrayWidth int `json:"maxArrayWidth"`
		KeyLength     int `json:"keyLength"`
	} `json:"config"`
	Ops []struct {
		Op    string  `json:"op"`
		Key   string  `json:"key"`
		Value *uint64 `json:"value"`
	} `json:"ops"`
	Root   string            `json:"root"`
	Blocks map[string]string `json:"blocks"`
}

// readTracker records every block fetched through it, so the reachable set
// of a tree can be captured by traversing it.
type readTracker struct {
	inner *mockBlocks
	seen  map[cid.Cid][]byte
}

func (rt *readTracker) Get(ctx context.Context, c cid.Cid) (block.Block, error) {
	b, err := rt.inner.Get(ctx, c)
	if err == nil {
		rt.seen[c] = b.RawData()
	}
	return b, err
}

func (rt *readTracker) Put(ctx context.Context, b block.Block) error {
	return rt.inner.Put(ctx, b)
}

func cborUint(v uint64) []byte {
	var out []byte
	w := &writerFunc{&out}
	cw := cbg.NewCborWriter(w)
	if err := cw.CborWriteHeader(cbg.MajUnsignedInt, v); err != nil {
		panic(err)
	}
	return out
}

type writerFunc struct{ out *[]byte }

func (w *writerFunc) Write(p []byte) (int, error) {
	*w.out = append(*w.out, p...)
	return len(p), nil
}

func TestRustFixtures(t *testing.T) {
	ctx := context.Background()
	paths, err := filepath.Glob("fixtures/*.json")
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no fixtures found; run fixtures/generator")

	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var f fixture
		require.NoError(t, json.Unmarshal(data, &f))

		t.Run(f.Name, func(t *testing.T) {
			t.Logf("fixture source: %s", f.Source)
			options := []Option{
				UseTreeBitWidth(f.Config.BitWidth),
				UseMinDataDepth(f.Config.MinDataDepth),
				UseMaxArrayWidth(f.Config.MaxArrayWidth),
				UseKeyLength(f.Config.KeyLength),
			}

			blocks := newMockBlocks()
			n, err := NewNode(cbor.NewCborStore(blocks), options...)
			require.NoError(t, err)

			for _, op := range f.Ops {
				key, err := hex.DecodeString(op.Key)
				require.NoError(t, err)
				switch op.Op {
				case "set":
					require.NoError(t, n.SetRaw(ctx, key, cborUint(*op.Value)))
				case "delete":
					_, err := n.Delete(ctx, key)
					require.NoError(t, err)
				default:
					t.Fatalf("unknown op %q", op.Op)
				}
			}

			root, err := n.Write(ctx)
			require.NoError(t, err)
			require.Equal(t, f.Root, root.String(), "root CID must match the Rust implementation")

			// capture our reachable set and require it to be byte-identical
			// to the fixture's
			tracker := &readTracker{inner: blocks, seen: make(map[cid.Cid][]byte)}
			reloaded, err := LoadNode(ctx, cbor.NewCborStore(tracker), root, options...)
			require.NoError(t, err)
			require.NoError(t, reloaded.ForEach(ctx, func([]byte, *cbg.Deferred) error { return nil }))

			require.Len(t, tracker.seen, len(f.Blocks), "reachable block count must match")
			for c, expectedHex := range f.Blocks {
				ci, err := cid.Parse(c)
				require.NoError(t, err)
				got, ok := tracker.seen[ci]
				require.True(t, ok, "missing block %s", c)
				require.Equal(t, expectedHex, hex.EncodeToString(got), "block %s must be byte-identical", c)
			}
		})
	}
}
