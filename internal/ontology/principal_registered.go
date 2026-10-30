package ontology

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/invopop/jsonschema"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// KindPrincipalRegistered is the ontology kind under which
// PrincipalRegisteredEvent is registered. A principalRegistered event is
// appended once at genesis (and on a later GitHub-anchor upgrade): it
// mints the principal — a concrete person entity — and records the
// public keys, key fingerprint, and optional GitHub trust anchor that let
// other writers verify the principal's signed events.
const KindPrincipalRegistered = "principalRegistered"

// VersionPrincipalRegistered is the semver-shaped schema version for
// PrincipalRegisteredEvent.
const VersionPrincipalRegistered = "v0.1.0"

// GitHub anchor strengths recorded on a principal. "ssh" means an SSHSIG
// attestation by a GitHub-published key verified at enrollment; "none"
// means the binding is absent or unverified (offline, no gh, or no
// published signing key). Mirrors identity.GitHubAnchorSSH/None.
const (
	GitHubAnchorSSH  = "ssh"
	GitHubAnchorNone = "none"
)

// GitHubAnchorPrefix is the scheme that prefixes a GitHub identity anchor
// stored in SameAs / the principals.github_login column ("github:<login>").
// Readers that want the bare login (e.g. to synthesize a no-reply commit
// email) must strip it; the value is stored with the prefix so the anchor
// namespace stays explicit and extensible to other identity providers.
const GitHubAnchorPrefix = "github:"

// PrincipalRegisteredEvent is the payload of a principalRegistered event.
// It binds a stable principal UID (a ULID, also the UID of the principal's
// person entity) to its ed25519 signing key and x25519 wrapping key, plus
// the optional GitHub identity anchor. The Go struct is the source of
// truth; the JSON Schema is reflected from it at first access and cached.
type PrincipalRegisteredEvent struct {
	// PrincipalUID is the ULID identifying the principal. It is also the
	// UID of the principal's person entity, so facts and relations can
	// point at it directly. Required.
	PrincipalUID string `json:"principalUid" jsonschema:"required,minLength=1,description=ULID of the principal (also its person entity UID). Required."`

	// DisplayName is the human-readable principal name. Required.
	DisplayName string `json:"displayName" jsonschema:"required,minLength=1,description=Human-readable principal name. Required."`

	// SignPublicKey is the base64-encoded ed25519 public key that verifies
	// the principal's signed events. Required.
	SignPublicKey string `json:"signPublicKey" jsonschema:"required,minLength=1,description=Base64 ed25519 public key. Required."`

	// WrapPublicKey is the base64-encoded x25519 public key used to seal
	// per-scope keys to this principal (teams envelope encryption).
	// Required (derived at genesis even though MVP does not yet wrap).
	WrapPublicKey string `json:"wrapPublicKey" jsonschema:"required,minLength=1,description=Base64 x25519 public key. Required."`

	// SigKeyID is the fingerprint of SignPublicKey ("kid:..."), the lookup
	// key into the enrolled-device table. Required.
	SigKeyID string `json:"sigKeyId" jsonschema:"required,minLength=1,description=Fingerprint of the ed25519 public key. Required."`

	// CreateTime is the instant the principal was registered, UTC. Required.
	CreateTime time.Time `json:"createTime" jsonschema:"required,format=date-time,description=When the principal was registered. RFC 3339 UTC. Required."`

	// SameAs is the GitHub identity anchor, "github:<login>". Optional;
	// absent when no GitHub identity was resolved (offline / unconfigured).
	SameAs string `json:"sameAs,omitempty" jsonschema:"description=GitHub identity anchor, e.g. github:octocat. Absent when unconfigured."`

	// GitHubUserID is the immutable GitHub numeric user id. Logins are
	// renameable, so this is the durable cross-reference. Optional.
	GitHubUserID int64 `json:"githubUserId,omitempty" jsonschema:"description=Immutable GitHub numeric user id."`

	// GitHubAnchor records the binding strength: "ssh" (a verified SSHSIG
	// attestation by a published key) or "none" (absent/unverified).
	// Optional; treated as "none" when empty.
	GitHubAnchor string `json:"githubAnchor,omitempty" jsonschema:"enum=ssh,enum=none,description=GitHub anchor strength: ssh (verified) or none."`

	// GitHubAttestation is the armored SSHSIG binding this principal's
	// ed25519 identity to the GitHub account, signed by a GitHub-published
	// key. Present only when GitHubAnchor is "ssh".
	GitHubAttestation string `json:"githubAttestation,omitempty" jsonschema:"description=Armored SSHSIG attestation by a GitHub-published key. Present when githubAnchor=ssh."`

	// GitHubAnchorKey is the authorized_keys line of the published key that
	// produced the attestation. Present only when GitHubAnchor is "ssh".
	GitHubAnchorKey string `json:"githubAnchorKey,omitempty" jsonschema:"description=authorized_keys line of the vouching published key. Present when githubAnchor=ssh."`
}

// principalRegisteredSchema lazily builds the schema once per process.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var principalRegisteredSchema = sync.OnceValue(buildPrincipalRegisteredSchema)

func buildPrincipalRegisteredSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&PrincipalRegisteredEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal principalRegistered schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindPrincipalRegistered,
		Version:    VersionPrincipalRegistered,
		Definition: raw,
		Provenance: "core",
	}
}

// PrincipalRegisteredSchema returns the cached kernel.Schema.
func PrincipalRegisteredSchema() kernel.Schema {
	return principalRegisteredSchema()
}

// RegisterPrincipalRegistered installs the PrincipalRegisteredEvent schema
// with o. Call once at process startup.
func RegisterPrincipalRegistered(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, principalRegisteredSchema())
}
