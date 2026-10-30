package ontology

import (
	"crypto/sha256"

	"github.com/oklog/ulid/v2"
)

// externalRefDedupSeparator joins the scope and uri into the dedup key. A byte
// (0x0A) that cannot appear in a scope token keeps the (scope, uri) pair
// unambiguous: ("project", "a\nb") and ("project\na", "b") never collide.
const externalRefDedupSeparator = "\n"

// ExternalRefUID derives the canonical external_ref ULID for a (scope, uri)
// pair. It is a pure function — the same pair always yields the same ULID — so
// the `reference` tool is mint-or-return: a repeat call for the same URI in the
// same scope resolves to the same entities/{uid} resource name instead of
// minting a duplicate. Baking the scope into the key makes dedup WITHIN scope
// only: the same URI in personal and project scope derives two distinct UIDs,
// so a personal-private pointer never collapses into a project-scope entity
// that renders to git.
//
// The 128-bit identity is the leading half of SHA-256(scope + "\n" + uri),
// rendered via ulid.String() — the same encoding SourceRefUID uses. String
// emits the top byte as Encoding[(id[0]&224)>>5], i.e. only id[0]'s high 3
// bits, so the first Crockford symbol is always 0–7 regardless of the hash
// bytes; ulid.Parse rejects only a leading symbol > '7' (ErrOverflow), so any
// 16-byte value round-trips unconditionally. The id is not time-ordered, which
// the entity projection does not rely on.
func ExternalRefUID(scope, uri string) ULID {
	sum := sha256.Sum256([]byte(scope + externalRefDedupSeparator + uri))
	var id ulid.ULID
	copy(id[:], sum[:len(id)])
	return ULID(id.String())
}
