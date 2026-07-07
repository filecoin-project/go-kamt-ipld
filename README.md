# go-kamt-ipld

A Go implementation of the Filecoin KAMT, the "fixed-size Keyed AMT": a
sharded IPLD map keyed by fixed-length byte strings, with level skipping.
The KAMT is a HAMT derivative that does not hash keys and lets links skip
runs of empty levels via extensions, designed for the EVM actor's contract
storage ([FIP-0054](https://github.com/filecoin-project/FIPs/blob/master/FIPS/fip-0054.md#contract-storage-kamt)),
where consecutive Solidity slots share long key prefixes.

Writes are wire-compatible, bit for bit, with the reference Rust
implementation
[`fvm_ipld_kamt`](https://github.com/filecoin-project/ref-fvm/tree/master/ipld/kamt).
The `fixtures/` directory holds cross-implementation vectors generated from
the Rust crate by `fixtures/generator`; the test suite replays them and
requires identical root CIDs and byte-identical blocks. The reader is
deliberately stricter than Rust's: it rejects a number of non-canonical
block shapes that no conformant writer produces (see the compatibility
contract in `doc.go`).

See `doc.go` for the format description, parameters and canonical form
rules, and [go-hamt-ipld](https://github.com/filecoin-project/go-hamt-ipld)
for the hashed sibling structure.

## Usage

```go
import (
    kamt "github.com/filecoin-project/go-kamt-ipld"
    cbor "github.com/ipfs/go-ipld-cbor"
)

// the EVM actor's contract storage parameters
n, err := kamt.NewNode(cbor.NewCborStore(bs),
    kamt.UseTreeBitWidth(5), kamt.UseMaxArrayWidth(1))

err = n.Set(ctx, key, value) // key is exactly 32 bytes
c, err := n.Write(ctx)       // flush and get the root CID

n, err = kamt.LoadNode(ctx, store, c,
    kamt.UseTreeBitWidth(5), kamt.UseMaxArrayWidth(1))
```

The configuration is not recorded in the serialized form; readers and
writers must agree on it out of band.

## Regenerating fixtures

```console
$ cd fixtures/generator && cargo run --release
```

## License

Dual MIT and Apache 2.
