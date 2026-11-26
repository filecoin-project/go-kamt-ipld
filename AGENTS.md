# go-kamt-ipld agent guide

go-kamt-ipld is the Go KAMT, the unhashed HAMT variant with extension links that stores EVM contract storage (FIP-0054). [ref-fvm](https://github.com/filecoin-project/ref-fvm)'s `ipld/kamt` is the format authority; this library must produce identical blocks and roots.

`doc.go` is the format specification, including the compatibility contract with the Rust reader. Read it before changing encoding, traversal or validation, and update it in the same PR.

## Format rules

- The blocks don't record bitwidth, bucket size, minimum data depth or key length; the caller supplies them. The EVM's parameters are set in builtin-actors' EVM actor, and any Go reader of contract storage must pass the same ones.
- Traversal and extension handling are ports of the Rust code, including behaviour that looks inefficient. Change them only with a matching change in Rust.
- This reader is stricter than Rust's in the ways `doc.go` lists. A new rejection rule must not reject blocks Rust writes, and goes on that list.

## Testing

- `go test ./...`. `TestRustFixtures` replays operation vectors generated from the Rust crate (`fixtures/*.json`) and requires identical roots and blocks; `TestChainExtractedFixtures` reserializes trees taken from chain (`fixtures/chain/`). `invalid_test.go` holds malformed and non-canonical blocks.
- A behaviour change needs a regenerated or new fixture: `cd fixtures/generator && cargo run --release`. The generator builds against the published `fvm_ipld_kamt`; point it at a local ref-fvm checkout to test an unreleased Rust change.

## Comments, commits and pull requests

- Write comments and docs year-zero, describing the code as it is; history goes in the commit message and PR description. A comment states what a maintainer can't read from the code: an invariant, a format constraint, a surprising cost.
- PR titles use Conventional Commits, with `!` before the colon for a change that breaks importers.
- Report suspected vulnerabilities as the [Filecoin security policy](https://github.com/filecoin-project/community/blob/master/SECURITY.md) describes, never in a public issue or PR.

## Maintaining agent context

Fix this file in the same PR as a change that makes it wrong. `CLAUDE.md` contains only `@AGENTS.md`, so Claude Code loads this file.

