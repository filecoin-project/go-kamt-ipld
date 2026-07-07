package kamt

import "errors"

// ErrMaxDepth is returned when a key would require traversal beyond the
// number of levels the fixed-size key can address. With well-formed keys of
// the configured length this is unreachable; it indicates a key of the wrong
// length or a corrupt structure.
var ErrMaxDepth = errors.New("attempted to traverse KAMT beyond key length")

// errZeroPointers is an internal sentinel raised when a node is found (or
// made) empty where the canonical form does not permit it. During deletes it
// signals that an artificially inserted link-only node (above minDataDepth)
// should be removed entirely.
var errZeroPointers = errors.New("KAMT node has no pointers")

// errInvalidBitLen is returned for bit reads outside the supported 1..8
// range.
var errInvalidBitLen = errors.New("invalid bit count, must be 1..8")
