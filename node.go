package kamt

import (
	"bytes"
	"context"
	"fmt"
	"io"

	cid "github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// Node is a single block of a KAMT: a bitfield recording which of the
// 2^bitWidth slots are occupied, and a compacted array with one Pointer per
// set bit. The root of a KAMT is a plain Node; no wrapper records the
// configuration, so a reader must know the parameters the structure was
// built with.
type Node struct {
	Bitfield Bitfield
	Pointers []*Pointer

	// context for operations, not serialized
	store cbor.IpldStore
	cfg   *config
}

func (n *Node) MarshalCBOR(w io.Writer) error {
	// guard against hand-built impossible states: the popcount contract is
	// what makes the compacted pointers array addressable
	if ones := n.Bitfield.countOnes(); ones != len(n.Pointers) {
		return fmt.Errorf("cannot serialize: %d pointers but %d bits set", len(n.Pointers), ones)
	}

	cw := cbg.NewCborWriter(w)
	if err := cw.CborWriteHeader(cbg.MajArray, 2); err != nil {
		return err
	}
	if err := n.Bitfield.MarshalCBOR(cw); err != nil {
		return err
	}
	if err := cw.CborWriteHeader(cbg.MajArray, uint64(len(n.Pointers))); err != nil {
		return err
	}
	for _, p := range n.Pointers {
		if err := p.MarshalCBOR(cw); err != nil {
			return err
		}
	}
	return nil
}

func (n *Node) UnmarshalCBOR(r io.Reader) error {
	cr := cbg.NewCborReader(r)
	maj, extra, err := cr.ReadHeader()
	if err != nil {
		return err
	}
	if maj != cbg.MajArray || extra != 2 {
		return fmt.Errorf("node must be a 2-element array")
	}
	if err := n.Bitfield.UnmarshalCBOR(cr); err != nil {
		return err
	}
	maj, extra, err = cr.ReadHeader()
	if err != nil {
		return err
	}
	if maj != cbg.MajArray {
		return fmt.Errorf("node pointers must be an array")
	}
	// the bitfield can address at most 256 slots (bitWidth is capped at 8)
	if extra > 256 {
		return fmt.Errorf("node pointers array too long (%d)", extra)
	}
	n.Pointers = make([]*Pointer, extra)
	for i := range n.Pointers {
		var p Pointer
		if err := p.UnmarshalCBOR(cr); err != nil {
			return err
		}
		n.Pointers[i] = &p
	}
	// A node occupies an entire block; trailing bytes mean the CID commits
	// to data this decode did not represent. The Rust decoder rejects them
	// too (serde_ipld_dagcbor TrailingData).
	var trailing [1]byte
	if c, _ := cr.Read(trailing[:]); c != 0 {
		return fmt.Errorf("unexpected trailing bytes after node")
	}
	return nil
}

// loadNode fetches a node block and verifies it is in canonical form for
// the given configuration and depth. Blocks that could not have been
// produced by a conformant writer are rejected rather than normalized.
func loadNode(ctx context.Context, cs cbor.IpldStore, cfg *config, c cid.Cid, depth int) (*Node, error) {
	var n Node
	if err := cs.Get(ctx, c, &n); err != nil {
		return nil, err
	}
	n.store = cs
	n.cfg = cfg

	if len(n.Pointers) > 1<<cfg.bitWidth {
		return nil, fmt.Errorf("number of pointers (%d) exceeds that allowed by the bitWidth (%d)",
			len(n.Pointers), 1<<cfg.bitWidth)
	}
	if ones := n.Bitfield.countOnes(); ones != len(n.Pointers) {
		return nil, fmt.Errorf("number of pointers (%d) doesn't match bitfield (%d)",
			len(n.Pointers), ones)
	}
	// Bits may only be set within the node's 2^bitWidth slots. The Rust
	// implementation does not check this (such bits address pointers no
	// lookup can reach), but no conformant writer sets them.
	if n.Bitfield.BitLen() > 1<<cfg.bitWidth {
		return nil, fmt.Errorf("bitfield has bits set beyond the %d slots addressable at bitWidth %d",
			1<<cfg.bitWidth, cfg.bitWidth)
	}
	// only a root node may be empty
	if len(n.Pointers) == 0 && depth != 0 {
		return nil, errZeroPointers
	}

	for _, p := range n.Pointers {
		if p.isShard() {
			if err := validateLink(cfg, p, depth); err != nil {
				return nil, err
			}
		} else if err := validateBucket(cfg, p, depth); err != nil {
			return nil, err
		}
	}

	// A non-root node that could be represented more compactly must have
	// been: a single-pointer node is spliced into its parent's link, and a
	// node whose buckets fit within one bucket is collapsed (the delete-path
	// rules in Pointer.clean). The Rust implementation does not check these
	// on load; no conformant writer produces them. The collapse rules are
	// suspended above minDataDepth, judged at the parent's logical depth;
	// physical depth is a lower bound on logical depth, so this guard never
	// misfires when extensions skip levels.
	if depth != 0 && depth-1 >= cfg.minDataDepth {
		if len(n.Pointers) == 1 {
			return nil, fmt.Errorf("single-pointer node should have been collapsed into its parent")
		}
		count := 0
		buckets := true
		for _, p := range n.Pointers {
			if p.isShard() {
				buckets = false
				break
			}
			count += len(p.KVs)
		}
		if buckets && count <= cfg.maxArrayWidth {
			return nil, fmt.Errorf("node holding %d entries in buckets should have been collapsed into a single parent bucket", count)
		}
	}

	return &n, nil
}

func validateBucket(cfg *config, p *Pointer, depth int) error {
	if depth < cfg.minDataDepth {
		return fmt.Errorf("values not allowed below the minimum data depth (%d < %d)",
			depth, cfg.minDataDepth)
	}
	if len(p.KVs) == 0 {
		return fmt.Errorf("empty bucket")
	}
	if len(p.KVs) > cfg.maxArrayWidth {
		return fmt.Errorf("too many items in bucket (%d > %d)", len(p.KVs), cfg.maxArrayWidth)
	}
	for i, kv := range p.KVs {
		// keys arrive in wire form: zeroless big-endian, at most keyLength
		// bytes. The zeroless requirement is part of canonical form; the
		// Rust implementation would accept a padded key here but can never
		// produce one.
		if len(kv.Key) > cfg.keyLength {
			return fmt.Errorf("key of %d bytes exceeds the key length (%d)", len(kv.Key), cfg.keyLength)
		}
		if len(kv.Key) > 0 && kv.Key[0] == 0 {
			return fmt.Errorf("key has leading zero bytes")
		}
		p.KVs[i].Key = padKey(kv.Key, cfg.keyLength)
	}
	for i := 1; i < len(p.KVs); i++ {
		if bytes.Compare(p.KVs[i-1].Key, p.KVs[i].Key) >= 0 {
			return fmt.Errorf("duplicate or unsorted keys in bucket")
		}
	}
	return nil
}

func validateLink(cfg *config, p *Pointer, depth int) error {
	if p.Link.Prefix().Codec != cid.DagCBOR {
		return fmt.Errorf("KAMT nodes must be DAG-CBOR, not codec %d", p.Link.Prefix().Codec)
	}
	if p.ext.length%cfg.bitWidth != 0 {
		return fmt.Errorf("extension length %d is not a multiple of the bitWidth %d",
			p.ext.length, cfg.bitWidth)
	}
	remaining := cfg.keyLength*8 - (depth+1)*cfg.bitWidth
	if p.ext.length >= remaining {
		return fmt.Errorf("extension length must be less than %d bits, was %d bits",
			remaining, p.ext.length)
	}
	// unused trailing bits of the final path byte must be zero; part of
	// canonical form, though the Rust implementation does not check it on
	// load (nor can it produce a violation)
	if r := p.ext.length % 8; r != 0 && len(p.ext.path) > 0 {
		if p.ext.path[len(p.ext.path)-1]&mkmask(8-r) != 0 {
			return fmt.Errorf("extension path has nonzero trailing bits")
		}
	}
	return nil
}

// insertPointer places a pointer at the slot for bit position idx,
// maintaining the compacted array order.
func (n *Node) insertPointer(idx int, p *Pointer) {
	i := indexForBitPos(idx, &n.Bitfield.Int)
	n.Bitfield.setBit(idx)
	n.Pointers = append(n.Pointers[:i], append([]*Pointer{p}, n.Pointers[i:]...)...)
}

// rmChild removes the pointer at compacted position cindex, occupying bit
// position idx.
func (n *Node) rmChild(cindex, idx int) *Pointer {
	p := n.Pointers[cindex]
	copy(n.Pointers[cindex:], n.Pointers[cindex+1:])
	n.Pointers = n.Pointers[:len(n.Pointers)-1]
	n.Bitfield.clearBit(idx)
	return p
}
