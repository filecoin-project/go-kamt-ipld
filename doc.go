/*
Package kamt implements the Filecoin KAMT, a "fixed-size Keyed AMT": a
sharded IPLD map keyed by fixed-length byte strings, with level skipping.
It is wire-compatible, bit for bit, with the reference Rust implementation
(fvm_ipld_kamt in ref-fvm), which defines the format; cross-implementation
fixtures generated from the Rust crate are replayed by this package's tests.

The KAMT is a derivative of the Filecoin HAMT with two structural
differences, both introduced to suit the EVM actor's contract storage,
currently the format's only production use (see FIP-0054):

  - Keys are not hashed. A key is a fixed-length byte string used directly
    as the traversal path, consumed bitWidth bits at a time from the most
    significant bit of the first byte. Solidity locates storage in
    contiguous runs of slots; hashing would scatter them, while direct
    keying co-locates them under shared prefixes.

  - Links carry extensions. Unhashed keys with long common prefixes would
    otherwise force long chains of intermediate nodes each holding a single
    pointer. A link's extension records the skipped portion of the key path
    so the link points directly at the next branching (or data-bearing)
    node, in the manner of a Patricia tree.

# Structure

A node is a bitfield over 2^bitWidth slots plus a compacted pointers array
holding one entry per set bit; a pointer at slot i of the bitfield lives at
position popcount(bitfield, up to i) of the array. Each pointer is either a
bucket of up to maxArrayWidth key-value pairs, sorted by key, or a link to
a child node with its extension.

Lookup (Find) walks the key's bits: at each node, take the next bitWidth
bits as a slot index; absent bit means absent key; a bucket is searched
directly; a link's extension must match the next ext-length bits of the
key exactly (any divergence means the key is absent), after which
traversal continues below.

Set inserts into buckets until they overflow, at which point the bucket's
entries and the new entry are pushed down into a new child node linked with
an extension covering the longest prefix they all still share. A key that
diverges partway along an existing extension splits it: a new node is
inserted at the divergence with two children, the original target under the
tail of the extension and the new entry, with the link above retaining the
head.

Delete reverses all of this exactly. A node left with a single pointer is
spliced out, merging the extensions above and below it (the reverse of a
split); a node whose buckets would fit within one bucket is collapsed into
one. The tree for a given set of entries is therefore canonical: the same
entries under the same parameters produce the same nodes and the same root
CID regardless of the order of operations that produced them. A reader
rejects, rather than normalizes, blocks violating the local wire
invariants listed under "Serialized form" below; global minimality (that
every collapse the entries permit has been performed) is a writer
obligation a reader cannot cheaply prove.

When minDataDepth is greater than zero, levels above it hold only links,
never buckets; entries that would land there are pushed down into
(otherwise empty) child nodes. This keeps frequently read upper nodes
small.

# Parameters

The serialized form records none of the configuration; every reader and
writer must supply identical parameters or the data will be unreadable, or
worse, writes will produce divergent CIDs.

  - bitWidth (1..8, default 8): bits of key consumed per level; nodes have
    2^bitWidth slots.
  - maxArrayWidth (default 3): entries a bucket may hold before push-down.
  - minDataDepth (default 0): shallowest level allowed to hold buckets.
  - keyLength (default 32): the fixed key length in bytes.

The EVM actor's contract storage (persistent and transient) uses bitWidth
5, maxArrayWidth 1, minDataDepth 0 and 32-byte big-endian U256 keys.

# Serialized form

Blocks are DAG-CBOR; Filecoin links them with Blake2b-256 CIDs, and links
must themselves be DAG-CBOR. As IPLD Schemas:

	type Node struct {
		bitfield Bytes       # minimal big-endian, no leading zero bytes
		pointers [Pointer]
	} representation tuple

	type Pointer union {
		| Bucket "v"
		| Link "l"
	} representation keyed

	type Link struct {
		node &Node
		extLength Int        # extension length in bits
		extPath Bytes        # skipped key bits, packed MSB-first
	} representation tuple

	type Bucket [KV]

	type KV struct {
		key Bytes            # zeroless: big-endian, leading zero bytes stripped
		value Any
	} representation tuple

Note the keyed (single-entry map) union, unlike the Filecoin HAMT v3's
kinded union. Bucket keys are stored zeroless, matching the serialization
of the EVM actor's U256 keys; this package accepts and returns full-length
keys and converts at the boundary. The extension path's bit length is
recorded separately because it need not be a whole number of bytes; unused
trailing bits of the final byte are zero.

Values are embedded as given and treated as opaque: readers check only
that a value is a single well-formed CBOR item, and writers using SetRaw
are responsible for the DAG-CBOR strictness of the bytes they supply.
Set routes through the same single-item validation; cbor-gen generated
marshalers produce strict output, but the strictness of a hand-written
CBORMarshaler is not verified.

# Compatibility contract

For conforming input this package writes blocks byte-identical to the
Rust implementation; the fixture suites (Rust-generated vectors and a
chain-extracted CAR) hold that equality. The accepted-block sets are not
identical: this reader deliberately rejects several non-canonical shapes
the Rust reader currently accepts, none of which a conformant writer
produces. Enumerated: non-minimal or out-of-range bitfield encodings;
non-zeroless bucket keys; extension paths with nonzero trailing bits or
a byte length disagreeing with the bit length; non-DAG-CBOR root CIDs
(the Rust store layer also admits plain CBOR); uncollapsed single-pointer
or bucket-collapsible non-root nodes. Conversely, node blocks followed by
trailing bytes are rejected by both (Go checks in Node.UnmarshalCBOR,
Rust in its decoder). Validation remains local per block: key membership
under the path leading to a bucket is not verified, so a maliciously
crafted tree can hold entries visible to ForEach that Find cannot reach;
full verification requires traversal-aware checking, which neither
implementation performs today.

Load validation enforces the local invariants of canonical form: pointer
count equal to the bitfield's popcount and within 2^bitWidth, no bits set
beyond the addressable slots, minimal bitfield encoding; no empty nodes
except a root with no entries at all; buckets non-empty, within
maxArrayWidth, strictly sorted, keys zeroless, and only at or below
minDataDepth; extension lengths a multiple of bitWidth and shorter than
the key bits remaining at their depth, with zeroed trailing path bits.

# Usage considerations

The KAMT provides no key privacy and no defence against adversarial key
choice: with no hashing, whoever chooses keys chooses the shape of the
tree. Extensions make deliberately deep paths cheap to represent (a shared
250-bit prefix costs one extension, not 50 levels), but the caller remains
responsible for key distribution in adversarial settings, as the EVM does
by deriving storage slots from Keccak-256 where user input is involved.

Mutations accumulate in memory until Flush or Write; blocks are written
bottom-up so links always address flushed children.
*/
package kamt
