package tools

// team_device_tools.go exposes per-device signing-key management as MCP tools:
// team_device_list (list enrolled devices for the local principal with
// revocation status) and team_device_revoke (append a deviceRevoked event).
// Both are thin adapters over team.Service's DevicesList / DeviceRevoke.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/team"
)

// TeamDeviceOps is the device-management surface the tools depend on.
// *team.Service satisfies it; tests pass a fake.
type TeamDeviceOps interface {
	DevicesList(ctx context.Context) (team.DevicesListResult, error)
	DeviceRevoke(ctx context.Context, deviceID, reason string) (team.DeviceRevokeResult, error)
}

// --- team_device_list ---

const teamDeviceListInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const teamDeviceListOutputSchema = `{
  "type": "object",
  "properties": {
    "devices": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "deviceId":      {"type": "string"},
          "principalUid":  {"type": "string"},
          "sigKeyId":      {"type": "string"},
          "deviceLabel":   {"type": "string"},
          "vouchSigKeyId": {"type": "string"},
          "createTime":    {"type": "string", "format": "date-time"},
          "revoked":       {"type": "boolean"},
          "revokedAt":     {"type": "string", "format": "date-time"},
          "revokedReason": {"type": "string"}
        },
        "required": ["deviceId", "principalUid", "sigKeyId", "createTime", "revoked"],
        "additionalProperties": false
      }
    }
  },
  "required": ["devices"],
  "additionalProperties": false
}`

// TeamDeviceList is the MCP tool that lists enrolled devices for the local
// principal.
type TeamDeviceList struct{ ops TeamDeviceOps }

// NewTeamDeviceList constructs the tool bound to ops.
func NewTeamDeviceList(ops TeamDeviceOps) *TeamDeviceList { return &TeamDeviceList{ops: ops} }

// Name implements mcp.Tool.
func (*TeamDeviceList) Name() string { return "team_device_list" }

// Description implements mcp.Tool.
func (*TeamDeviceList) Description() string {
	return "List all devices enrolled under the local principal's identity, including revocation status. Each device entry shows its signing-key fingerprint (sigKeyId), optional label, create time, and whether it has been revoked."
}

// InputSchema implements mcp.Tool.
func (*TeamDeviceList) InputSchema() json.RawMessage {
	return json.RawMessage(teamDeviceListInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*TeamDeviceList) OutputSchema() json.RawMessage {
	return json.RawMessage(teamDeviceListOutputSchema)
}

type teamDeviceListDeviceOutput struct {
	DeviceID      string  `json:"deviceId"`
	PrincipalUID  string  `json:"principalUid"`
	SigKeyID      string  `json:"sigKeyId"`
	DeviceLabel   string  `json:"deviceLabel,omitempty"`
	VouchSigKeyID string  `json:"vouchSigKeyId,omitempty"`
	CreateTime    string  `json:"createTime"`
	Revoked       bool    `json:"revoked"`
	RevokedAt     *string `json:"revokedAt,omitempty"`
	RevokedReason string  `json:"revokedReason,omitempty"`
}

type teamDeviceListOutput struct {
	Devices []teamDeviceListDeviceOutput `json:"devices"`
}

// Invoke implements mcp.Tool.
func (t *TeamDeviceList) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	result, err := t.ops.DevicesList(ctx)
	if err != nil {
		return nil, mapTeamError("team_device_list", err)
	}

	out := teamDeviceListOutput{
		Devices: make([]teamDeviceListDeviceOutput, 0, len(result.Devices)),
	}
	for _, d := range result.Devices {
		entry := teamDeviceListDeviceOutput{
			DeviceID:      d.DeviceID,
			PrincipalUID:  d.PrincipalUID,
			SigKeyID:      d.SigKeyID,
			DeviceLabel:   d.DeviceLabel,
			VouchSigKeyID: d.VouchSigKeyID,
			CreateTime:    d.CreateTime.Format(time.RFC3339),
			Revoked:       d.RevokedAt != nil,
		}
		if d.RevokedAt != nil {
			s := d.RevokedAt.Format(time.RFC3339)
			entry.RevokedAt = &s
			entry.RevokedReason = d.RevokedReason
		}
		out.Devices = append(out.Devices, entry)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("team_device_list: marshal output: %w", err))
	}
	return raw, nil
}

// --- team_device_revoke ---

const teamDeviceRevokeInputSchema = `{
  "type": "object",
  "properties": {
    "deviceId": {"type": "string", "description": "ULID of the device to revoke. Required."},
    "reason":   {"type": "string", "description": "Optional free-form reason for the revocation (audit trail)."}
  },
  "required": ["deviceId"],
  "additionalProperties": false
}`

const teamDeviceRevokeOutputSchema = `{
  "type": "object",
  "properties": {
    "eventUid":     {"type": "string", "description": "UID of the appended deviceRevoked event."},
    "deviceId":     {"type": "string", "description": "ULID of the revoked device."},
    "principalUid": {"type": "string", "description": "ULID of the device's owning principal."},
    "status":       {"type": "string", "enum": ["revoked"], "description": "Always \"revoked\" on success."}
  },
  "required": ["eventUid", "deviceId", "principalUid", "status"],
  "additionalProperties": false
}`

// TeamDeviceRevoke is the MCP tool that appends a deviceRevoked event.
type TeamDeviceRevoke struct{ ops TeamDeviceOps }

// NewTeamDeviceRevoke constructs the tool bound to ops.
func NewTeamDeviceRevoke(ops TeamDeviceOps) *TeamDeviceRevoke { return &TeamDeviceRevoke{ops: ops} }

// Name implements mcp.Tool.
func (*TeamDeviceRevoke) Name() string { return "team_device_revoke" }

// Description implements mcp.Tool.
func (*TeamDeviceRevoke) Description() string {
	return "Revoke an enrolled device by its ULID. Appends a deviceRevoked event to the team scope. After revocation, any event authored by this device with a UID greater than the revocation event's UID is dropped on all peers (convergent fold). The revoker must be the device's own principal, or a team admin/owner."
}

// InputSchema implements mcp.Tool.
func (*TeamDeviceRevoke) InputSchema() json.RawMessage {
	return json.RawMessage(teamDeviceRevokeInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*TeamDeviceRevoke) OutputSchema() json.RawMessage {
	return json.RawMessage(teamDeviceRevokeOutputSchema)
}

type teamDeviceRevokeInput struct {
	DeviceID string `json:"deviceId"`
	Reason   string `json:"reason,omitempty"`
}

type teamDeviceRevokeOutput struct {
	EventUID     string `json:"eventUid"`
	DeviceID     string `json:"deviceId"`
	PrincipalUID string `json:"principalUid"`
	Status       string `json:"status"`
}

// Invoke implements mcp.Tool.
func (t *TeamDeviceRevoke) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in teamDeviceRevokeInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.DeviceID == "" {
		return nil, mcp.NewValidationError("deviceId is required", nil)
	}

	result, err := t.ops.DeviceRevoke(ctx, in.DeviceID, in.Reason)
	if err != nil {
		return nil, mapTeamError("team_device_revoke", err)
	}

	raw, err := json.Marshal(teamDeviceRevokeOutput{
		EventUID:     result.EventUID,
		DeviceID:     result.DeviceID,
		PrincipalUID: result.PrincipalUID,
		Status:       "revoked",
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("team_device_revoke: marshal output: %w", err))
	}
	return raw, nil
}
