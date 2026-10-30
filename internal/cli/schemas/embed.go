// Package schemas provides the embedded CLI JSON Schema files.
// Each file is named after the command it describes:
//
//   - error.json: AIP-193 error envelope shape.
//   - list_envelope.json: generic pagination envelope.
//   - schema_predicates_response.json: maz schema predicates response.
package schemas

import "embed"

// FS is the embedded file system containing all schemas/cli/*.json files.
//
//go:embed *.json
var FS embed.FS
