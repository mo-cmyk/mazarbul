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

// KindDeviceRevoked is the event kind that records a device revocation.
// Once a deviceRevoked event is applied, any event authored by the named
// device with a UID strictly greater than the revocation's UID is treated
// as invalid and dropped during graft and rebuild. The rule is position-pure:
// the verdict depends only on ULID ordering in the event set, so every
// peer reaches the same conclusion regardless of the arrival order of the
// revocation and the events it governs.
const KindDeviceRevoked = "deviceRevoked"

// VersionDeviceRevoked is the semver-shaped schema version.
const VersionDeviceRevoked = "v0.1.0"

// DeviceRevokedEvent is the payload of a deviceRevoked event.
type DeviceRevokedEvent struct {
	// DeviceID is the ULID of the device being revoked. Required.
	DeviceID string `json:"deviceId" jsonschema:"required,minLength=1,description=ULID of the revoked device. Required."`

	// SigKeyID is the fingerprint of the revoked device's signing key.
	// Required. Allows revocation by key fingerprint when the device ULID is
	// unknown to the revoker.
	SigKeyID string `json:"sigKeyId" jsonschema:"required,minLength=1,description=Fingerprint of the revoked device's signing key. Required."`

	// PrincipalUID is the ULID of the principal that owned the device.
	// Required.
	PrincipalUID string `json:"principalUid" jsonschema:"required,minLength=1,description=ULID of the device's owning principal. Required."`

	// RevokedAt is the wall-clock instant the revocation was requested, UTC.
	// Required.
	RevokedAt time.Time `json:"revokedAt" jsonschema:"required,format=date-time,description=When the revocation was requested. RFC 3339 UTC. Required."`

	// Reason is an optional free-form revocation reason for audit purposes.
	Reason string `json:"reason,omitempty" jsonschema:"description=Optional free-form reason for the revocation."`
}

// deviceRevokedSchema lazily builds the schema once per process.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var deviceRevokedSchema = sync.OnceValue(buildDeviceRevokedSchema)

func buildDeviceRevokedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&DeviceRevokedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal deviceRevoked schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindDeviceRevoked,
		Version:    VersionDeviceRevoked,
		Definition: raw,
		Provenance: "core",
	}
}

// DeviceRevokedSchema returns the cached kernel.Schema.
func DeviceRevokedSchema() kernel.Schema {
	return deviceRevokedSchema()
}

// RegisterDeviceRevoked installs the DeviceRevokedEvent schema with o.
// Call once at process startup.
func RegisterDeviceRevoked(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, deviceRevokedSchema())
}
