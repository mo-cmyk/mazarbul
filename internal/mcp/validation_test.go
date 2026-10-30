package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompileSchema(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name: "empty schema yields passthrough",
			raw:  "",
		},
		{
			name: "valid object schema",
			raw:  `{"type":"object","properties":{"fact":{"type":"string"}},"required":["fact"]}`,
		},
		{
			name:    "malformed json",
			raw:     `{"type":}`,
			wantErr: "parse input schema",
		},
		{
			name:    "invalid schema rejected at compile",
			raw:     `{"type":"not-a-real-type"}`,
			wantErr: "compile input schema",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := compileSchema("test-tool", json.RawMessage(tc.raw))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("compileSchema(%q) = nil, want error containing %q", tc.raw, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("compileSchema(%q): %v", tc.raw, err)
			}
			if v == nil {
				t.Fatalf("compileSchema returned nil validator without error")
			}
		})
	}
}

func TestSchemaValidator_Validate(t *testing.T) {
	t.Parallel()
	const strictSchema = `{
		"type":"object",
		"properties":{"fact":{"type":"string"}},
		"required":["fact"],
		"additionalProperties":false
	}`

	cases := []struct {
		name     string
		schema   string
		args     string
		wantCode ToolErrorCode
	}{
		{
			name:   "valid args pass",
			schema: strictSchema,
			args:   `{"fact":"the sky is blue"}`,
		},
		{
			name:     "missing required field",
			schema:   strictSchema,
			args:     `{}`,
			wantCode: CodeValidation,
		},
		{
			name:     "wrong type",
			schema:   strictSchema,
			args:     `{"fact":123}`,
			wantCode: CodeValidation,
		},
		{
			name:     "extra property rejected",
			schema:   strictSchema,
			args:     `{"fact":"x","junk":true}`,
			wantCode: CodeValidation,
		},
		{
			name:     "malformed args",
			schema:   strictSchema,
			args:     `not json`,
			wantCode: CodeValidation,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := compileSchema("validate-test", json.RawMessage(tc.schema))
			if err != nil {
				t.Fatalf("compileSchema: %v", err)
			}
			got := v.Validate(json.RawMessage(tc.args))
			if tc.wantCode == "" {
				if got != nil {
					t.Fatalf("Validate(%q) = %v, want nil", tc.args, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("Validate(%q) = nil, want code %q", tc.args, tc.wantCode)
			}
			if got.Code != tc.wantCode {
				t.Fatalf("Validate code = %q, want %q", got.Code, tc.wantCode)
			}
		})
	}
}

func TestSchemaValidator_NilReceiver(t *testing.T) {
	t.Parallel()
	var v *schemaValidator
	if got := v.Validate(json.RawMessage(`{}`)); got != nil {
		t.Fatalf("nil validator returned %v, want nil", got)
	}
	empty := &schemaValidator{}
	if got := empty.Validate(json.RawMessage(`{"anything":true}`)); got != nil {
		t.Fatalf("passthrough validator returned %v, want nil", got)
	}
}

func TestValidationDetails_NonValidationError(t *testing.T) {
	t.Parallel()
	if got := validationDetails(nil); got != nil {
		t.Fatalf("validationDetails(nil) = %s, want nil", got)
	}
}
