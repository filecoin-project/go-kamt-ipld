package kamt

import (
	"bytes"
	"context"
	"slices"

	cid "github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// KV is a key-value pair held in a bucket. In memory Key is always the full
// configured key length; on the wire it is stored zeroless (big-endian with
// leading zero bytes stripped), matching the serialization of the EVM
// actor's U256 keys.
type KV struct {
	Key   []byte
	Value *cbg.Deferred
}

// Pointer occupies one set bit of a node's bitfield and is either a bucket
// of key-value pairs or a link to a child node. A link carries an extension
// recording any part of the key path it skips.
type Pointer struct {
	KVs  []*KV
	Link cid.Cid
	ext  extension

	// cached child node; whether loaded from Link or newly created (dirty)
	cache *Node
	// dirty is set when cache has been modified and Link is stale or unset;
	// cleared by Flush
	dirty bool
}

// isShard reports whether this pointer leads to a child node rather than
// holding a bucket.
func (p *Pointer) isShard() bool {
	return p.cache != nil || p.Link.Defined()
}

// Extension reports the key path a link skips over: its length in bits (a
// multiple of the tree bitWidth, zero when the link skips nothing) and the
// packed MSB-first path bytes. Intended for structural inspection of
// trees; the returned path is a copy. Meaningless for bucket pointers,
// which return (0, nil).
func (p *Pointer) Extension() (lengthBits int, path []byte) {
	if p.ext.isEmpty() {
		return 0, nil
	}
	return p.ext.length, append([]byte(nil), p.ext.path...)
}

// zerolessKey returns the key with leading zero bytes stripped, the wire
// form for bucket keys. A zero key encodes as an empty byte string.
func zerolessKey(k []byte) []byte {
	i := 0
	for i < len(k) && k[i] == 0 {
		i++
	}
	return k[i:]
}

// padKey left-pads a wire-form key back to the full configured length.
func padKey(k []byte, keyLength int) []byte {
	if len(k) == keyLength {
		return k
	}
	out := make([]byte, keyLength)
	copy(out[keyLength-len(k):], k)
	return out
}

// loadChild loads the child node this pointer links to, caching it for
// subsequent access. The node is validated at the given depth.
func (p *Pointer) loadChild(ctx context.Context, cs cbor.IpldStore, cfg *config, depth int) (*Node, error) {
	if p.cache != nil {
		return p.cache, nil
	}
	child, err := loadNode(ctx, cs, cfg, p.Link, depth)
	if err != nil {
		return nil, err
	}
	p.cache = child
	return child, nil
}

// clean restores canonical form after a delete has modified the child node
// below this pointer. Any node that could be represented more compactly must
// be: a single-pointer node is spliced out (merging extensions), and a node
// whose buckets would all fit within one bucket is collapsed into one.
// Returns errZeroPointers if the child node has become empty.
func (p *Pointer) clean(cfg *config, depth int) error {
	n := p.cache
	switch w := len(n.Pointers); {
	case w == 0:
		return errZeroPointers
	case depth < cfg.minDataDepth:
		// We are in the shallows where only links are allowed, no key-value
		// pairs. As long as links point at non-empty nodes they can stay: the
		// rules below would either move key-value pairs up or undo a split,
		// and neither happens above the minimum data depth.
		return nil
	case w == 1:
		// Node has only one pointer; splice it into this one. If the child's
		// single pointer is itself a link, this pointer can link straight to
		// its target by merging the two extensions around the child's index.
		sub := n.Pointers[0]
		if !sub.isShard() {
			p.KVs = sub.KVs
			p.Link = cid.Undef
			p.ext = extension{}
			p.cache = nil
			p.dirty = false
			return nil
		}
		ext, err := unsplitExt(cfg, &n.Bitfield, &p.ext, &sub.ext)
		if err != nil {
			return err
		}
		p.Link = sub.Link
		p.ext = ext
		p.cache = sub.cache
		p.dirty = sub.dirty
		return nil
	case w <= cfg.maxArrayWidth:
		// If the child's pointers are all buckets whose entries would fit in
		// a single bucket, collapse them into one here.
		count := 0
		for _, sub := range n.Pointers {
			if sub.isShard() {
				return nil
			}
			count += len(sub.KVs)
		}
		if count > cfg.maxArrayWidth {
			return nil
		}

		var kvs []*KV
		for _, sub := range n.Pointers {
			kvs = append(kvs, sub.KVs...)
		}
		// Bucket entries are ordered by key; the child's buckets were laid
		// out by key path so a straight sort restores the canonical order.
		sortKVs(kvs)

		p.KVs = kvs
		p.Link = cid.Undef
		p.ext = extension{}
		p.cache = nil
		p.dirty = false
		return nil
	default:
		return nil
	}
}

func sortKVs(kvs []*KV) {
	slices.SortFunc(kvs, func(a, b *KV) int { return bytes.Compare(a.Key, b.Key) })
}

// unsplitExt merges the extension of a pointer with that of the single child
// pointer below it, reconstructing the path through the child node's one
// occupied index.
func unsplitExt(cfg *config, bf *Bitfield, parentExt, childExt *extension) (extension, error) {
	idx := bf.lastOneIdx()
	if idx < 0 {
		return extension{}, errZeroPointers
	}
	idxExt := extensionFromIdx(idx, cfg.bitWidth)
	return unsplitExtensions(parentExt, &idxExt, childExt)
}
