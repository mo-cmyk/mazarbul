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

// KindDeviceEnrolled is the ontology kind under which DeviceEnrolledEvent
// is registered. A deviceEnrolled event binds a device's ed25519 signing
// public key to a principal. At genesis the single device self-signs its
// enrollment with the principal's own key (TOFU root); the schema's
// Signature field leaves room for multi-device signed chains without a
// break. The dispatcher projects these events into the `devices` table the
// verifier consults by sigKeyID.
const KindDeviceEnrolled = "deviceEnrolled"

// VersionDeviceEnrolled is the semver-shaped schema version.
// The v0.2.0 revision adds the optional VouchSigKeyID and VouchSignature
// fields for subkey devices (devices that use a random signing key rather
// than the Watchword-derived principal key). Absent vouch fields mean a
// legacy device-0 enrollment — still fully valid.
const VersionDeviceEnrolled = "v0.2.0"

// DeviceEnrolledEvent is the payload of a deviceEnrolled event.
type DeviceEnrolledEvent struct {
	// DeviceID is the ULID of the device being enrolled. Required.
	DeviceID string `json:"deviceId" jsonschema:"required,minLength=1,description=ULID of the enrolled device. Required."`

	// PrincipalUID is the ULID of the principal this device belongs to.
	// Required.
	PrincipalUID string `json:"principalUid" jsonschema:"required,minLength=1,description=ULID of the owning principal. Required."`

	// SignPublicKey is the base64 ed25519 public key of THIS device.
	// Required.
	SignPublicKey string `json:"signPublicKey" jsonschema:"required,minLength=1,description=Base64 ed25519 public key of this device. Required."`

	// SigKeyID is the fingerprint of SignPublicKey ("kid:..."), the lookup
	// key the verifier uses. Required.
	SigKeyID string `json:"sigKeyId" jsonschema:"required,minLength=1,description=Fingerprint of this device's ed25519 public key. Required."`

	// DeviceLabel is an optional human-readable label (e.g. hostname).
	DeviceLabel string `json:"deviceLabel,omitempty" jsonschema:"description=Human-readable device label, e.g. hostname."`

	// Signature is the base64 ed25519 signature over the canonical
	// enrollment bytes (deviceId|principalUid|signPublicKey|sigKeyId). At
	// genesis the principal's own key signs it (TOFU). Required.
	Signature string `json:"signature" jsonschema:"required,minLength=1,description=Base64 ed25519 signature over the enrollment bytes. Required."`

	// CreateTime is the instant the device was enrolled, UTC. Required.
	CreateTime time.Time `json:"createTime" jsonschema:"required,format=date-time,description=When the device was enrolled. RFC 3339 UTC. Required."`

	// VouchSigKeyID is the fingerprint of the PRINCIPAL key that signed the
	// vouch (absent for device-0 / legacy enrollments). When present, the
	// dispatcher verifies VouchSignature against this key from the principals
	// table, proving the principal authorized this device subkey.
	VouchSigKeyID string `json:"vouchSigKeyId,omitempty" jsonschema:"description=Fingerprint of the principal key that signed the enrollment vouch. Present only for subkey devices."`

	// VouchSignature is the base64 ed25519 signature by the PRINCIPAL key
	// over DeviceVouchBytes(deviceId, signPublicKey). Absent for legacy
	// device-0 enrollments; required for subkey devices.
	VouchSignature string `json:"vouchSignature,omitempty" jsonschema:"description=Base64 principal-key signature over the device vouch bytes. Present only for subkey devices."`
}

// deviceEnrolledSchema lazily builds the schema once per process.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var deviceEnrolledSchema = sync.OnceValue(buildDeviceEnrolledSchema)

func buildDeviceEnrolledSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&DeviceEnrolledEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal deviceEnrolled schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindDeviceEnrolled,
		Version:    VersionDeviceEnrolled,
		Definition: raw,
		Provenance: "core",
	}
}

// DeviceEnrolledSchema returns the cached kernel.Schema.
func DeviceEnrolledSchema() kernel.Schema {
	return deviceEnrolledSchema()
}

// RegisterDeviceEnrolled installs the DeviceEnrolledEvent schema with o.
// Call once at process startup.
func RegisterDeviceEnrolled(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, deviceEnrolledSchema())
}

// DeviceEnrollmentBytes returns the canonical bytes the deviceEnrolled
// Signature covers. Exposed so the genesis signer and any future verifier
// produce identical input. Domain-separated and field-delimited; the
// fields are fixed-meaning so a simple NUL-join is unambiguous.
func DeviceEnrollmentBytes(deviceID, principalUID, signPublicKeyB64, sigKeyID string) []byte {
	return []byte("mazarbul-device-enroll-v1\x00" +
		deviceID + "\x00" + principalUID + "\x00" + signPublicKeyB64 + "\x00" + sigKeyID)
}

// DeviceVouchBytes returns the canonical bytes the principal-key vouch
// signature covers in VouchSignature. The vouch binds a specific device
// subkey to its device ID, proving the principal authorized that exact key
// for that device. Domain-separated separately from DeviceEnrollmentBytes so
// the two signatures cannot be confused.
func DeviceVouchBytes(deviceID, devicePubKeyB64 string) []byte {
	return []byte("mazarbul-device-vouch-v1\x00" + deviceID + "\x00" + devicePubKeyB64)
}
