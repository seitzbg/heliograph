package configstore

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
)

// newUUID returns a random RFC 4122 version-4 UUID string. Generated with crypto/rand
// (not math/rand — this needs to be safe to call concurrently and unguessable, and the
// project avoids external uuid dependencies), so no import beyond the standard library.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// MintNewIDs stamps a fresh, opaque UUID onto every host-bearing node in doc that has no
// id yet. This is the counterpart to MigrateStampIDs' birth-path minter: that migration
// mints id = path for EXISTING nodes, because id already equals the string the store
// keys their history under, so nothing needs re-keying. A brand-new node must NOT get
// its path minted as its id — a node freshly created at a path a moved node vacated
// would otherwise collide with that moved node's frozen birth-path id. So MintNewIDs
// reuses the same walker (stampIDs) with a minter that ignores the path entirely and
// draws a random UUID instead.
//
// Call this on every ConfigApply, server-side, before validating/persisting the applied
// doc — never trust the client (the UI) to mint the id itself.
//
// Idempotent: a node that already carries a non-empty id (existing/migrated, or minted
// by an earlier apply) is left untouched, because stampIDs only stamps id-less
// host-bearing nodes.
func MintNewIDs(doc json.RawMessage) (json.RawMessage, bool) {
	return stampIDs(doc, func(_ string) string { return newUUID() })
}
