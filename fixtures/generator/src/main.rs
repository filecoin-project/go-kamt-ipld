//! Generates cross-implementation test fixtures for go-kamt-ipld.
//!
//! Each fixture applies a deterministic sequence of set/delete operations to
//! a `fvm_ipld_kamt::Kamt` and records the configuration, the operations, the
//! resulting root CID and every block written, hex-encoded. A conformant
//! implementation replaying the operations must produce byte-identical
//! blocks and the same root.
//!
//! Keys are 32 bytes, serialized zeroless (big-endian, leading zero bytes
//! stripped) exactly as the EVM actor's U256 keys are; values are u64.

use std::borrow::Cow;
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::fmt;
use std::fs;
use std::path::Path;

use cid::Cid;
use fvm_ipld_blockstore::Blockstore;
use fvm_ipld_kamt::{AsHashedKey, Config, HashedKey, Kamt};
use serde::{Deserialize, Serialize};

/// Blockstore that remembers everything written to it and records which
/// blocks are read, so the reachable set of a tree can be captured by
/// re-traversing it.
#[derive(Default)]
struct MapStore {
    data: RefCell<BTreeMap<Cid, Vec<u8>>>,
    read: RefCell<BTreeMap<Cid, Vec<u8>>>,
}

impl Blockstore for &MapStore {
    fn get(&self, k: &Cid) -> anyhow::Result<Option<Vec<u8>>> {
        let data = self.data.borrow().get(k).cloned();
        if let Some(d) = &data {
            self.read.borrow_mut().insert(*k, d.clone());
        }
        Ok(data)
    }
    fn put_keyed(&self, k: &Cid, block: &[u8]) -> anyhow::Result<()> {
        self.data.borrow_mut().insert(*k, block.to_vec());
        Ok(())
    }
}

/// 32-byte key serialized the way the EVM actor serializes U256: zeroless
/// big-endian bytes.
#[derive(Clone, PartialEq, Eq, PartialOrd, Ord, Debug)]
struct Key([u8; 32]);

impl Serialize for Key {
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        let zeros = self.0.iter().take_while(|&&b| b == 0).count();
        serializer.serialize_bytes(&self.0[zeros..])
    }
}

impl<'de> Deserialize<'de> for Key {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        struct Visitor;
        impl serde::de::Visitor<'_> for Visitor {
            type Value = Key;
            fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
                write!(f, "at most 32 bytes")
            }
            fn visit_bytes<E: serde::de::Error>(self, v: &[u8]) -> Result<Key, E> {
                if v.len() > 32 {
                    return Err(E::invalid_length(v.len(), &self));
                }
                let mut k = [0u8; 32];
                k[32 - v.len()..].copy_from_slice(v);
                Ok(Key(k))
            }
        }
        deserializer.deserialize_bytes(Visitor)
    }
}

struct Identity;

impl AsHashedKey<Key, 32> for Identity {
    fn as_hashed_key(key: &Key) -> Cow<'_, HashedKey<32>> {
        Cow::Borrowed(&key.0)
    }
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct FixtureConfig {
    bit_width: u32,
    min_data_depth: u32,
    max_array_width: usize,
    key_length: usize,
}

#[derive(Serialize)]
struct Op {
    op: &'static str, // "set" | "delete"
    key: String,      // 32 bytes, hex
    #[serde(skip_serializing_if = "Option::is_none")]
    value: Option<u64>,
}

/// Recorded in every fixture so consumers know which reference produced
/// it. Update when the generator's fvm_ipld_kamt dependency changes.
const SOURCE: &str = concat!("fvm_ipld_kamt ", "0.4.6", " (crates.io)");

#[derive(Serialize)]
struct Fixture {
    name: String,
    source: &'static str,
    config: FixtureConfig,
    ops: Vec<Op>,
    root: String,
    blocks: BTreeMap<String, String>,
}

/// xorshift64*; deterministic across implementations, no dependency
struct Rng(u64);

impl Rng {
    fn next(&mut self) -> u64 {
        let mut x = self.0;
        x ^= x >> 12;
        x ^= x << 25;
        x ^= x >> 27;
        self.0 = x;
        x.wrapping_mul(0x2545F4914F6CDD1D)
    }

    fn key(&mut self) -> [u8; 32] {
        let mut k = [0u8; 32];
        for c in k.chunks_mut(8) {
            c.copy_from_slice(&self.next().to_be_bytes());
        }
        k
    }
}

fn key_from_u64(i: u64) -> [u8; 32] {
    let mut k = [0u8; 32];
    k[24..].copy_from_slice(&i.to_be_bytes());
    k
}

fn key_add(base: &[u8; 32], offset: u64) -> [u8; 32] {
    let mut k = *base;
    let mut carry = offset;
    for i in (0..32).rev() {
        if carry == 0 {
            break;
        }
        let sum = k[i] as u64 + (carry & 0xff);
        k[i] = sum as u8;
        carry = (carry >> 8) + (sum >> 8);
    }
    k
}

enum PlanOp {
    Set([u8; 32], u64),
    Delete([u8; 32]),
}

fn run(name: &str, conf: Config, plan: Vec<PlanOp>) -> Fixture {
    let store = MapStore::default();
    let mut kamt: Kamt<&MapStore, Key, u64, Identity> =
        Kamt::new_with_config(&store, conf.clone());

    let mut ops = Vec::new();
    for op in &plan {
        match op {
            PlanOp::Set(k, v) => {
                kamt.set(Key(*k), *v).expect("set");
                ops.push(Op { op: "set", key: hex::encode(k), value: Some(*v) });
            }
            PlanOp::Delete(k) => {
                kamt.delete(&Key(*k)).expect("delete");
                ops.push(Op { op: "delete", key: hex::encode(k), value: None });
            }
        }
    }

    let root = kamt.flush().expect("flush");

    // Capture only the blocks reachable from the final root, by fully
    // re-traversing a freshly loaded copy through the read-recording store.
    // Intermediate states left behind by the churn are not part of the
    // fixture; a replaying implementation is only expected to produce the
    // final tree.
    store.read.borrow_mut().clear();
    let reloaded: Kamt<&MapStore, Key, u64, Identity> =
        Kamt::load_with_config(&root, &store, conf.clone()).expect("load");
    reloaded.for_each(|_, _| Ok(())).expect("traverse");
    let blocks: BTreeMap<String, String> = store
        .read
        .borrow()
        .iter()
        .map(|(c, d)| (c.to_string(), hex::encode(d)))
        .collect();

    Fixture {
        name: name.to_string(),
        source: SOURCE,
        config: FixtureConfig {
            bit_width: conf.bit_width,
            min_data_depth: conf.min_data_depth,
            max_array_width: conf.max_array_width,
            key_length: 32,
        },
        ops,
        root: root.to_string(),
        blocks,
    }
}

fn evm_config() -> Config {
    Config { bit_width: 5, min_data_depth: 0, max_array_width: 1 }
}

fn main() {
    let out_dir = Path::new(env!("CARGO_MANIFEST_DIR")).parent().unwrap().to_path_buf();

    let mut fixtures = Vec::new();

    fixtures.push(run("evm-empty", evm_config(), vec![]));

    fixtures.push(run(
        "evm-single",
        evm_config(),
        vec![PlanOp::Set(key_from_u64(1), 42)],
    ));

    // includes the zero key, which serializes as an empty byte string
    fixtures.push(run(
        "evm-zero-key",
        evm_config(),
        vec![PlanOp::Set([0u8; 32], 1), PlanOp::Set(key_from_u64(1), 2)],
    ));

    // two keys differing only in the final byte force the deepest split
    fixtures.push(run(
        "evm-deep-split",
        evm_config(),
        vec![
            PlanOp::Set(key_from_u64(0x10), 1),
            PlanOp::Set(key_from_u64(0x11), 2),
        ],
    ));

    fixtures.push(run("evm-sequential", evm_config(), {
        (0..200).map(|i| PlanOp::Set(key_from_u64(i), i * 7)).collect()
    }));

    fixtures.push(run("evm-churn", evm_config(), {
        let mut plan: Vec<PlanOp> =
            (0..150).map(|i| PlanOp::Set(key_from_u64(i), i)).collect();
        for i in (0..150).step_by(3) {
            plan.push(PlanOp::Delete(key_from_u64(i)));
        }
        for i in (0..150).step_by(5) {
            plan.push(PlanOp::Set(key_from_u64(i), i + 1000));
        }
        plan
    }));

    fixtures.push(run("evm-random", evm_config(), {
        let mut rng = Rng(0xDEADBEEF);
        (0..100).map(|i| PlanOp::Set(rng.key(), i)).collect()
    }));

    // Solidity-style layout: clusters of consecutive slots at random bases
    fixtures.push(run("evm-solidity-clusters", evm_config(), {
        let mut rng = Rng(0xCAFE);
        let mut plan = Vec::new();
        for _ in 0..8 {
            let base = rng.key();
            for off in 0..20 {
                plan.push(PlanOp::Set(key_add(&base, off), off));
            }
        }
        plan
    }));

    fixtures.push(run(
        "defaults-random",
        Config::default(),
        {
            let mut rng = Rng(0x1234);
            let mut plan: Vec<PlanOp> = (0..120).map(|i| PlanOp::Set(rng.key(), i)).collect();
            let mut rng = Rng(0x1234);
            for i in 0..120u64 {
                let k = rng.key();
                if i % 4 == 0 {
                    plan.push(PlanOp::Delete(k));
                }
            }
            plan
        },
    ));

    fixtures.push(run(
        "bw4-mindata1",
        Config { bit_width: 4, min_data_depth: 1, max_array_width: 3 },
        {
            let mut rng = Rng(0x5555);
            let mut plan: Vec<PlanOp> = (0..60)
                .map(|i| {
                    if i % 2 == 0 {
                        PlanOp::Set(key_from_u64(i), i)
                    } else {
                        PlanOp::Set(rng.key(), i)
                    }
                })
                .collect();
            for i in (0..60).step_by(4) {
                plan.push(PlanOp::Delete(key_from_u64(i)));
            }
            plan
        },
    ));

    fixtures.push(run(
        "bw2-bucket1",
        Config { bit_width: 2, min_data_depth: 0, max_array_width: 1 },
        (0..40).map(|i| PlanOp::Set(key_from_u64(i), i * 3)).collect(),
    ));

    for f in &fixtures {
        let path = out_dir.join(format!("{}.json", f.name));
        fs::write(&path, serde_json::to_string_pretty(f).unwrap()).unwrap();
        println!("{}: root {} ({} blocks, {} ops)", f.name, f.root, f.blocks.len(), f.ops.len());
    }
}
