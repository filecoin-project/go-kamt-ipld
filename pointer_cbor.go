package kamt

import (
	"fmt"
	"io"

	cid "github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// A Pointer serializes as a single-entry map tagged by variant, matching the
// serde enum representation used by the Rust implementation:
//
//	{"v": [KV...]}                          a bucket
//	{"l": [CID, extLenBits, extPathBytes]}  a link with its extension
//
// Note this differs from the Filecoin HAMT v3, which uses a kinded union
// (bare CID or bare array) for its pointers.

const (
	// decode-time sanity caps; the real (configuration-aware) limits are
	// enforced by loadNode validation
	maxDecodeKVs    = 1024
	maxDecodeKeyLen = 64
	maxDecodeExtLen = 512 // bits; keys are at most 64 bytes
)

func (t *Pointer) MarshalCBOR(w io.Writer) error {
	if t.dirty {
		return fmt.Errorf("cannot serialize a dirty pointer, Flush first")
	}
	// guard against hand-built impossible states silently losing data
	if t.isShard() && len(t.KVs) > 0 {
		return fmt.Errorf("pointer cannot be both a link and a bucket")
	}
	if !t.isShard() && len(t.KVs) == 0 {
		return fmt.Errorf("pointer must be a link or a non-empty bucket")
	}

	cw := cbg.NewCborWriter(w)

	if err := cw.CborWriteHeader(cbg.MajMap, 1); err != nil {
		return err
	}

	if t.isShard() {
		if err := writeVariantKey(cw, 'l'); err != nil {
			return err
		}
		if err := cw.CborWriteHeader(cbg.MajArray, 3); err != nil {
			return err
		}
		if err := cbg.WriteCid(cw, t.Link); err != nil {
			return err
		}
		if err := cw.CborWriteHeader(cbg.MajUnsignedInt, uint64(t.ext.length)); err != nil {
			return err
		}
		return cbg.WriteByteArray(cw, t.ext.path)
	}

	if err := writeVariantKey(cw, 'v'); err != nil {
		return err
	}
	if err := cw.CborWriteHeader(cbg.MajArray, uint64(len(t.KVs))); err != nil {
		return err
	}
	for _, kv := range t.KVs {
		if err := kv.MarshalCBOR(cw); err != nil {
			return err
		}
	}
	return nil
}

func (t *Pointer) UnmarshalCBOR(r io.Reader) error {
	*t = Pointer{}

	cr := cbg.NewCborReader(r)

	maj, extra, err := cr.ReadHeader()
	if err != nil {
		return err
	}
	if maj != cbg.MajMap || extra != 1 {
		return fmt.Errorf("pointer must be a single-entry map")
	}

	variant, err := readVariantKey(cr)
	if err != nil {
		return err
	}

	switch variant {
	case 'v':
		maj, extra, err := cr.ReadHeader()
		if err != nil {
			return err
		}
		if maj != cbg.MajArray {
			return fmt.Errorf("pointer \"v\" entry must be an array of key-value pairs")
		}
		if extra > maxDecodeKVs {
			return fmt.Errorf("pointer bucket too long (%d)", extra)
		}
		t.KVs = make([]*KV, extra)
		for i := range t.KVs {
			var kv KV
			if err := kv.UnmarshalCBOR(cr); err != nil {
				return err
			}
			t.KVs[i] = &kv
		}
		return nil

	case 'l':
		maj, extra, err := cr.ReadHeader()
		if err != nil {
			return err
		}
		if maj != cbg.MajArray || extra != 3 {
			return fmt.Errorf("pointer \"l\" entry must be a 3-element array")
		}

		maj, extra, err = cr.ReadHeader()
		if err != nil {
			return err
		}
		if maj != cbg.MajTag || extra != 42 {
			return fmt.Errorf("expected tag 42 for child node link")
		}
		ba, err := cbg.ReadByteArray(cr, 512)
		if err != nil {
			return err
		}
		c, err := bufToCid(ba)
		if err != nil {
			return err
		}
		t.Link = c

		maj, extra, err = cr.ReadHeader()
		if err != nil {
			return err
		}
		if maj != cbg.MajUnsignedInt {
			return fmt.Errorf("extension length must be an unsigned integer")
		}
		if extra > maxDecodeExtLen {
			return fmt.Errorf("extension too long (%d bits)", extra)
		}
		path, err := cbg.ReadByteArray(cr, maxDecodeExtLen/8)
		if err != nil {
			return err
		}
		// The bit length and the byte path must agree; a length demanding
		// more bits than the path holds is unreadable.
		if len(path) != (int(extra)+7)/8 {
			return fmt.Errorf("extension path is %d bytes, expected %d for %d bits",
				len(path), (int(extra)+7)/8, extra)
		}
		t.ext = extension{length: int(extra), path: path}
		return nil

	default:
		return fmt.Errorf("unknown pointer variant %q", variant)
	}
}

func writeVariantKey(cw *cbg.CborWriter, variant byte) error {
	if err := cw.CborWriteHeader(cbg.MajTextString, 1); err != nil {
		return err
	}
	_, err := cw.Write([]byte{variant})
	return err
}

func readVariantKey(cr *cbg.CborReader) (byte, error) {
	maj, extra, err := cr.ReadHeader()
	if err != nil {
		return 0, err
	}
	if maj != cbg.MajTextString || extra != 1 {
		return 0, fmt.Errorf("pointer variant key must be a 1-character string")
	}
	var b [1]byte
	if _, err := io.ReadFull(cr, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func (kv *KV) MarshalCBOR(w io.Writer) error {
	cw := cbg.NewCborWriter(w)
	if err := cw.CborWriteHeader(cbg.MajArray, 2); err != nil {
		return err
	}
	// keys are stored zeroless: big-endian with leading zero bytes stripped
	if err := cbg.WriteByteArray(cw, zerolessKey(kv.Key)); err != nil {
		return err
	}
	return kv.Value.MarshalCBOR(cw)
}

func (kv *KV) UnmarshalCBOR(r io.Reader) error {
	cr := cbg.NewCborReader(r)
	maj, extra, err := cr.ReadHeader()
	if err != nil {
		return err
	}
	if maj != cbg.MajArray || extra != 2 {
		return fmt.Errorf("key-value pair must be a 2-element array")
	}
	key, err := cbg.ReadByteArray(cr, maxDecodeKeyLen)
	if err != nil {
		return err
	}
	// The key remains in wire form until node validation pads it back to the
	// configured length.
	kv.Key = key
	kv.Value = new(cbg.Deferred)
	return kv.Value.UnmarshalCBOR(cr)
}

// from https://github.com/whyrusleeping/cbor-gen/blob/211df3b9e24c6e0d0c338b440e6ab4ab298505b2/utils.go#L530
func bufToCid(buf []byte) (cid.Cid, error) {
	if len(buf) == 0 {
		return cid.Undef, fmt.Errorf("undefined CID")
	}
	if len(buf) < 2 {
		return cid.Undef, fmt.Errorf("DAG-CBOR serialized CIDs must have at least two bytes")
	}
	if buf[0] != 0 {
		return cid.Undef, fmt.Errorf("DAG-CBOR serialized CIDs must have binary multibase")
	}
	return cid.Cast(buf[1:])
}
