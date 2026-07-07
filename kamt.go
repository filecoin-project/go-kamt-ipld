package kamt

import (
	"bytes"
	"context"
	"fmt"

	cid "github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// NewNode creates a new empty KAMT root with the given IPLD store and
// options.
func NewNode(cs cbor.IpldStore, options ...Option) (*Node, error) {
	cfg := defaultConfig()
	for _, option := range options {
		if err := option(cfg); err != nil {
			return nil, err
		}
	}
	return &Node{store: cs, cfg: cfg}, nil
}

// LoadNode loads a KAMT root from the IPLD store. The options must describe
// the same configuration the structure was written with; none of it is
// recorded in the serialized form.
func LoadNode(ctx context.Context, cs cbor.IpldStore, c cid.Cid, options ...Option) (*Node, error) {
	cfg := defaultConfig()
	for _, option := range options {
		if err := option(cfg); err != nil {
			return nil, err
		}
	}
	// The root CID is part of the wire identity, held to the same rule as
	// child links. (The Rust store layer also accepts plain-CBOR roots;
	// Filecoin state never uses them.)
	if c.Prefix().Codec != cid.DagCBOR {
		return nil, fmt.Errorf("KAMT root must be DAG-CBOR, not codec %d", c.Prefix().Codec)
	}
	return loadNode(ctx, cs, cfg, c, 0)
}

func (n *Node) checkKey(k []byte) error {
	if len(k) != n.cfg.keyLength {
		return fmt.Errorf("key must be exactly %d bytes, got %d", n.cfg.keyLength, len(k))
	}
	return nil
}

// Find navigates to where key `k` should exist. If the key is not found,
// returns false. If found, returns true and, if `out` is non-nil, decodes
// the value into it.
func (n *Node) Find(ctx context.Context, k []byte, out cbg.CBORUnmarshaler) (bool, error) {
	if err := n.checkKey(k); err != nil {
		return false, err
	}
	return n.getValue(ctx, newHashBits(k), 0, k, func(kv *KV) error {
		if out == nil {
			return nil
		}
		return out.UnmarshalCBOR(bytes.NewReader(kv.Value.Raw))
	})
}

// FindRaw performs the same function as Find but returns a copy of the raw
// value bytes.
func (n *Node) FindRaw(ctx context.Context, k []byte) (bool, []byte, error) {
	if err := n.checkKey(k); err != nil {
		return false, nil, err
	}
	var value []byte
	found, err := n.getValue(ctx, newHashBits(k), 0, k, func(kv *KV) error {
		value = bytes.Clone(kv.Value.Raw)
		return nil
	})
	return found, value, err
}

// Set adds or overwrites the value at key `k`.
func (n *Node) Set(ctx context.Context, k []byte, v cbg.CBORMarshaler) error {
	d, err := wrapValue(v)
	if err != nil {
		return err
	}
	return n.SetRaw(ctx, k, d.Raw)
}

// SetRaw adds or overwrites pre-encoded value bytes at key `k`. The bytes
// must hold exactly one well-formed CBOR item and are copied. Well-formed
// is all that is checked: DAG-CBOR strictness (canonical integer widths, no
// indefinite lengths, tag restrictions) is not enforced and remains the
// caller's responsibility; non-strict value bytes are embedded, and
// committed to by CIDs, exactly as given. Set, which encodes through
// cbor-gen, always produces strict values.
func (n *Node) SetRaw(ctx context.Context, k []byte, raw []byte) error {
	if err := n.checkKey(k); err != nil {
		return err
	}
	// scanning the item into a Deferred both validates it and copies it
	d := new(cbg.Deferred)
	br := bytes.NewReader(raw)
	if err := d.UnmarshalCBOR(br); err != nil {
		return fmt.Errorf("value is not a well-formed CBOR item: %w", err)
	}
	if br.Len() != 0 {
		return fmt.Errorf("value has %d trailing bytes after its CBOR item", br.Len())
	}
	kc := make([]byte, len(k))
	copy(kc, k)
	_, _, err := n.modifyValue(ctx, newHashBits(kc), 0, kc, d, true)
	return err
}

// SetIfAbsent adds the value at key `k` only if the key has no value
// already, returning whether it was added.
func (n *Node) SetIfAbsent(ctx context.Context, k []byte, v cbg.CBORMarshaler) (bool, error) {
	if err := n.checkKey(k); err != nil {
		return false, err
	}
	d, err := wrapValue(v)
	if err != nil {
		return false, err
	}
	kc := make([]byte, len(k))
	copy(kc, k)
	_, modified, err := n.modifyValue(ctx, newHashBits(kc), 0, kc, d, false)
	return modified, err
}

// Delete removes the entry at key `k`, returning whether it was present.
// Deletion restores the canonical form for the remaining data, which may
// collapse child nodes and merge extensions back together. If an error is
// returned the in-memory tree may have been partially modified and should
// be discarded; a true return alongside an error means the entry was
// removed before the failure.
func (n *Node) Delete(ctx context.Context, k []byte) (bool, error) {
	if err := n.checkKey(k); err != nil {
		return false, err
	}
	deleted, err := n.rmValue(ctx, newHashBits(k), 0, k)
	return deleted != nil, err
}

// ForEach calls f on every key-value pair in the KAMT, in key order. Keys
// are presented at their full configured length; values as raw bytes. The
// value is shared with the tree and must be treated as read-only. This
// performs a full traversal, loading every reachable node.
func (n *Node) ForEach(ctx context.Context, f func(k []byte, val *cbg.Deferred) error) error {
	return n.forEachAt(ctx, 0, f)
}

func (n *Node) forEachAt(ctx context.Context, depth int, f func(k []byte, val *cbg.Deferred) error) error {
	for _, p := range n.Pointers {
		if p.isShard() {
			child, err := p.loadChild(ctx, n.store, n.cfg, depth+1)
			if err != nil {
				return err
			}
			if err := child.forEachAt(ctx, depth+1, f); err != nil {
				return err
			}
		} else {
			for _, kv := range p.KVs {
				if err := f(bytes.Clone(kv.Key), kv.Value); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// NodeInfo describes one node visited by ForEachNode.
type NodeInfo struct {
	// CID is the link the parent pointer holds for this node; undefined for
	// the node ForEachNode was called on.
	CID cid.Cid
	// Depth is the physical depth: nodes traversed from the root, which is
	// depth 0. Extensions do not contribute.
	Depth int
	// LogicalDepth counts key levels consumed to reach this node, including
	// levels skipped by extensions. Equal to Depth in a tree with no
	// extensions.
	LogicalDepth int
	// Node is the visited node. Read-only: it is shared with the tree.
	Node *Node
}

// ForEachNode calls f on every node in the KAMT, parents before children,
// children in key order. Structural inspection companion to ForEach: the
// pointers of each visited node expose buckets, links and extensions.
// Intended for trees loaded from a store; an unflushed tree has undefined
// CIDs on dirty links. This performs a full traversal, loading every
// reachable node.
func (n *Node) ForEachNode(ctx context.Context, f func(info NodeInfo) error) error {
	return n.forEachNodeAt(ctx, NodeInfo{Node: n}, f)
}

func (n *Node) forEachNodeAt(ctx context.Context, info NodeInfo, f func(info NodeInfo) error) error {
	if err := f(info); err != nil {
		return err
	}
	for _, p := range n.Pointers {
		if !p.isShard() {
			continue
		}
		child, err := p.loadChild(ctx, n.store, n.cfg, info.Depth+1)
		if err != nil {
			return err
		}
		childInfo := NodeInfo{
			CID:          p.Link,
			Depth:        info.Depth + 1,
			LogicalDepth: info.LogicalDepth + 1 + p.ext.length/n.cfg.bitWidth,
			Node:         child,
		}
		if err := child.forEachNodeAt(ctx, childInfo, f); err != nil {
			return err
		}
	}
	return nil
}

// Flush writes all modified nodes below this one to the IPLD store and
// updates the links that point to them.
func (n *Node) Flush(ctx context.Context) error {
	for _, p := range n.Pointers {
		if p.cache != nil && p.dirty {
			if err := p.cache.Flush(ctx); err != nil {
				return err
			}
			c, err := n.store.Put(ctx, p.cache)
			if err != nil {
				return err
			}
			p.Link = c
			p.dirty = false
		}
	}
	return nil
}

// Write flushes this node and its children and writes the root, returning
// its CID.
func (n *Node) Write(ctx context.Context) (cid.Cid, error) {
	if err := n.Flush(ctx); err != nil {
		return cid.Undef, err
	}
	return n.store.Put(ctx, n)
}

func wrapValue(v cbg.CBORMarshaler) (*cbg.Deferred, error) {
	// as in go-hamt-ipld, a nil value stores as CBOR null
	if v == nil {
		return &cbg.Deferred{Raw: cbg.CborNull}, nil
	}
	var buf bytes.Buffer
	if err := v.MarshalCBOR(&buf); err != nil {
		return nil, err
	}
	return &cbg.Deferred{Raw: buf.Bytes()}, nil
}

//-----------------------------------------------------------------------------
// Traversal internals. These follow the Rust implementation
// (ref-fvm/ipld/kamt) closely, including its depth bookkeeping: nodes are
// always load-validated at parent depth + 1, while the logical depth passed
// onward accounts for levels skipped by extensions.

// matchExtension checks the key against a link's extension. A full match
// (which includes the no-extension case) reports how many levels the
// extension skips; a partial match reports how many bits matched before
// divergence, which is where the extension will have to be split. Matched
// bits are consumed from hk.
func matchExtension(hk *hashBits, bitWidth int, ext *extension) (full bool, skipped, matched int, err error) {
	if ext.isEmpty() {
		return true, 0, 0, nil
	}
	matched, err = ext.longestMatch(hk, bitWidth)
	if err != nil {
		return false, 0, 0, err
	}
	return matched == ext.length, matched / bitWidth, matched, nil
}

func (n *Node) getValue(ctx context.Context, hk *hashBits, depth int, k []byte, cb func(*KV) error) (bool, error) {
	idx, err := hk.next(n.cfg.bitWidth)
	if err != nil {
		return false, err
	}

	if !n.Bitfield.testBit(idx) {
		return false, nil
	}

	cindex := indexForBitPos(idx, &n.Bitfield.Int)
	child := n.Pointers[cindex]

	if !child.isShard() {
		for _, kv := range child.KVs {
			if bytes.Equal(kv.Key, k) {
				return true, cb(kv)
			}
		}
		return false, nil
	}

	// The child is loaded before the extension is matched, mirroring the
	// Rust get path exactly (node.rs get_value), even though matching first
	// would let a divergent key prove absence without the block fetch (as
	// the mutation paths do). Reordering would change which malformed
	// blocks a lookup surfaces errors for; a spec change should precede it.
	node, err := child.loadChild(ctx, n.store, n.cfg, depth+1)
	if err != nil {
		return false, err
	}
	full, _, _, err := matchExtension(hk, n.cfg.bitWidth, &child.ext)
	if err != nil {
		return false, err
	}
	if !full {
		return false, nil
	}
	return node.getValue(ctx, hk, depth+1, k, cb)
}

// modifyValue adds or replaces the value at the key, returning the previous
// value if any, and whether anything changed (an overwrite with an identical
// value does not count as a change and will not dirty the path).
func (n *Node) modifyValue(ctx context.Context, hk *hashBits, depth int, k []byte, v *cbg.Deferred, overwrite bool) (*cbg.Deferred, bool, error) {
	idx, err := hk.next(n.cfg.bitWidth)
	if err != nil {
		return nil, false, err
	}

	// nothing at this index yet: insert a new bucket, or, above the minimum
	// data depth, a chain of link-only nodes leading down to one
	if !n.Bitfield.testBit(idx) {
		if n.cfg.minDataDepth <= depth {
			n.insertPointer(idx, &Pointer{KVs: []*KV{{Key: k, Value: v}}})
		} else {
			sub := &Node{store: n.store, cfg: n.cfg}
			if _, _, err := sub.modifyValue(ctx, hk, depth+1, k, v, overwrite); err != nil {
				return nil, false, err
			}
			n.insertPointer(idx, &Pointer{cache: sub, dirty: true})
		}
		return nil, true, nil
	}

	cindex := indexForBitPos(idx, &n.Bitfield.Int)
	child := n.Pointers[cindex]

	if child.isShard() {
		full, skipped, matched, err := matchExtension(hk, n.cfg.bitWidth, &child.ext)
		if err != nil {
			return nil, false, err
		}
		if full {
			node, err := child.loadChild(ctx, n.store, n.cfg, depth+1)
			if err != nil {
				return nil, false, err
			}
			old, modified, err := node.modifyValue(ctx, hk, depth+1+skipped, k, v, overwrite)
			if err != nil {
				return nil, false, err
			}
			if modified {
				child.dirty = true
			}
			return old, modified, nil
		}
		// the key diverges partway along the extension: split it there
		if err := n.splitExtension(child, hk, matched, k, v); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}

	// a bucket: update in place, push down into a new node if full, or
	// insert in order
	for _, kv := range child.KVs {
		if bytes.Equal(kv.Key, k) {
			if !overwrite {
				return nil, false, nil
			}
			valueChanged := !bytes.Equal(kv.Value.Raw, v.Raw)
			old := kv.Value
			kv.Value = v
			return old, valueChanged, nil
		}
	}

	if len(child.KVs) >= n.cfg.maxArrayWidth {
		// The bucket is full. Create a new child node holding the existing
		// entries plus the new one, linked with an extension covering the
		// longest prefix all of them share beyond this level.
		kvs := child.KVs
		keys := make([][]byte, len(kvs))
		for i, kv := range kvs {
			keys[i] = kv.Key
		}

		ext, err := longestCommonPrefix(hk, n.cfg.bitWidth, keys)
		if err != nil {
			return nil, false, err
		}
		skipped := ext.length / n.cfg.bitWidth
		consumed := hk.consumed

		sub := &Node{store: n.store, cfg: n.cfg}
		old, modified, err := sub.modifyValue(ctx, hk, depth+1+skipped, k, v, overwrite)
		if err != nil {
			return nil, false, err
		}
		for _, kv := range kvs {
			if _, _, err := sub.modifyValue(ctx, newHashBitsAt(kv.Key, consumed), depth+1+skipped, kv.Key, kv.Value, overwrite); err != nil {
				return nil, false, err
			}
		}

		child.KVs = nil
		child.cache = sub
		child.dirty = true
		child.ext = ext
		return old, modified, nil
	}

	pos := len(child.KVs)
	for i, kv := range child.KVs {
		if bytes.Compare(kv.Key, k) > 0 {
			pos = i
			break
		}
	}
	child.KVs = append(child.KVs[:pos], append([]*KV{{Key: k, Value: v}}, child.KVs[pos:]...)...)
	return nil, true, nil
}

// splitExtension handles a key that diverges partway along a link's
// extension. A new node is inserted at the divergence point with two
// children: the link's original target under the remainder of the extension,
// and the new key-value pair.
func (n *Node) splitExtension(child *Pointer, hk *hashBits, matched int, k []byte, v *cbg.Deferred) error {
	head, idxExt, tail, err := child.ext.split(matched, n.cfg.bitWidth)
	if err != nil {
		return err
	}
	idx, err := idxExt.pathBits().next(n.cfg.bitWidth)
	if err != nil {
		return err
	}

	midway := &Node{store: n.store, cfg: n.cfg}
	// point at the link's original target, under the tail of the extension
	midway.insertPointer(idx, &Pointer{
		Link:  child.Link,
		ext:   tail,
		cache: child.cache,
		dirty: child.dirty,
	})
	// insert the new value at the next index of its key
	vidx, err := hk.next(n.cfg.bitWidth)
	if err != nil {
		return err
	}
	midway.insertPointer(vidx, &Pointer{KVs: []*KV{{Key: k, Value: v}}})

	// replace the link with one pointing at the midway node
	child.Link = cid.Undef
	child.KVs = nil
	child.ext = head
	child.cache = midway
	child.dirty = true
	return nil
}

func (n *Node) rmValue(ctx context.Context, hk *hashBits, depth int, k []byte) (*cbg.Deferred, error) {
	idx, err := hk.next(n.cfg.bitWidth)
	if err != nil {
		return nil, err
	}

	if !n.Bitfield.testBit(idx) {
		return nil, nil
	}

	cindex := indexForBitPos(idx, &n.Bitfield.Int)
	child := n.Pointers[cindex]

	if child.isShard() {
		full, skipped, _, err := matchExtension(hk, n.cfg.bitWidth, &child.ext)
		if err != nil {
			return nil, err
		}
		if !full {
			return nil, nil
		}
		node, err := child.loadChild(ctx, n.store, n.cfg, depth+1)
		if err != nil {
			return nil, err
		}
		deleted, err := node.rmValue(ctx, hk, depth+1+skipped, k)
		if err != nil {
			return nil, err
		}
		if deleted != nil {
			child.dirty = true
			// restore canonical form; an artificially inserted link-only
			// node that is now empty is removed outright
			removeChild, err := cleanChild(child, n.cfg, depth)
			if err != nil {
				// the entry is already gone: report the deletion alongside
				// the error so the caller knows the tree was modified
				return deleted, err
			}
			if removeChild {
				n.rmChild(cindex, idx)
			}
		}
		return deleted, nil
	}

	for i, kv := range child.KVs {
		if bytes.Equal(kv.Key, k) {
			if len(child.KVs) == 1 {
				n.rmChild(cindex, idx)
			} else {
				child.KVs = append(child.KVs[:i], child.KVs[i+1:]...)
			}
			return kv.Value, nil
		}
	}
	return nil, nil
}

// cleanChild applies the canonical-form collapse rules to a modified child
// pointer. Returns true when the child has become an empty node that should
// be removed from its parent entirely, which only happens to the link-only
// nodes inserted above minDataDepth.
func cleanChild(child *Pointer, cfg *config, depth int) (bool, error) {
	err := child.clean(cfg, depth)
	if err == errZeroPointers && depth < cfg.minDataDepth {
		return true, nil
	}
	return false, err
}
