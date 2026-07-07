package kamt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"testing"

	block "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/stretchr/testify/require"
)

// Chain-extracted fixtures: KAMTs captured from real Filecoin network
// state (lotus-shed eth storage-dump --car), CARv1 files holding every
// block of a contract's storage tree. Unlike the generated fixtures,
// these testify to what the network actually accepts: every block was
// written by the Rust implementation inside the FVM and committed to by
// consensus.
//
// sessionkeyregistry-calibnet.car: FOC SessionKeyRegistry
// (0x518411c2062E119Aaf7A8B12A2eDf9a939347655) contract storage at
// calibnet epoch 3867219; 1510 slots, keccak-distributed keys.
func TestChainExtractedFixtures(t *testing.T) {
	fixtures := []struct {
		file    string
		entries int
	}{
		{"fixtures/chain/sessionkeyregistry-calibnet.car", 1510},
	}
	for _, fx := range fixtures {
		t.Run(fx.file, func(t *testing.T) {
			roots, blocks := readCARv1(t, fx.file)
			require.Len(t, roots, 1)

			mb := newMockBlocks()
			for c, data := range blocks {
				blk, err := block.NewBlockWithCid(data, c)
				require.NoError(t, err)
				require.NoError(t, mb.Put(context.Background(), blk))
			}
			cs := cbor.NewCborStore(mb)

			// Load with full canonical-form validation, count entries.
			n, err := LoadNode(context.Background(), cs, roots[0],
				UseTreeBitWidth(5), UseMaxArrayWidth(1))
			require.NoError(t, err)
			entries := 0
			require.NoError(t, n.ForEach(context.Background(), func(k []byte, _ *cbg.Deferred) error {
				entries++
				return nil
			}))
			require.Equal(t, fx.entries, entries)

			// Re-serialize every node and require byte identity with the
			// chain: proves this codec round-trips real FVM-written blocks.
			visited := 0
			require.NoError(t, n.ForEachNode(context.Background(), func(info NodeInfo) error {
				c := info.CID
				if !c.Defined() {
					c = roots[0]
				}
				var buf bytes.Buffer
				if err := info.Node.MarshalCBOR(&buf); err != nil {
					return err
				}
				require.Equal(t, blocks[c], buf.Bytes(), "node %s must re-serialize byte-identically", c)
				visited++
				return nil
			}))
			require.Equal(t, len(blocks), visited, "every block in the CAR is a reachable node")
		})
	}
}

// readCARv1 is a minimal CARv1 reader: varint-framed sections, a DAG-CBOR
// header {version, roots}, then (CID, data) blocks. Hand-rolled to keep
// go-car out of the module's dependencies.
func readCARv1(t *testing.T, path string) ([]cid.Cid, map[cid.Cid][]byte) {
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close() //nolint:errcheck
	br := bufio.NewReader(f)

	section := func() ([]byte, bool) {
		length, err := binary.ReadUvarint(br)
		if err == io.EOF {
			return nil, false
		}
		require.NoError(t, err)
		buf := make([]byte, length)
		_, err = io.ReadFull(br, buf)
		require.NoError(t, err)
		return buf, true
	}

	header, ok := section()
	require.True(t, ok, "missing CAR header")
	roots := parseCARHeader(t, header)

	blocks := make(map[cid.Cid][]byte)
	for {
		sec, ok := section()
		if !ok {
			break
		}
		n, c, err := cid.CidFromBytes(sec)
		require.NoError(t, err)
		blocks[c] = sec[n:]
	}
	return roots, blocks
}

// parseCARHeader decodes the DAG-CBOR map {"roots": [links], "version": 1}.
func parseCARHeader(t *testing.T, raw []byte) []cid.Cid {
	cr := cbg.NewCborReader(bytes.NewReader(raw))
	maj, pairs, err := cr.ReadHeader()
	require.NoError(t, err)
	require.EqualValues(t, cbg.MajMap, maj)
	var roots []cid.Cid
	for range pairs {
		key, err := cbg.ReadString(cr)
		require.NoError(t, err)
		switch key {
		case "roots":
			maj, count, err := cr.ReadHeader()
			require.NoError(t, err)
			require.EqualValues(t, cbg.MajArray, maj)
			for range count {
				c, err := cbg.ReadCid(cr)
				require.NoError(t, err)
				roots = append(roots, c)
			}
		case "version":
			maj, v, err := cr.ReadHeader()
			require.NoError(t, err)
			require.EqualValues(t, cbg.MajUnsignedInt, maj)
			require.EqualValues(t, 1, v)
		default:
			t.Fatalf("unexpected CAR header key %q", key)
		}
	}
	return roots
}
