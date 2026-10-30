package schemapack

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// compileJSONSchema validates raw as a JSON Schema document using the
// santhosh-tekuri compiler with format assertions enabled — identical
// settings to the dispatcher's compile-before-materialise guard and the
// CLI's compilePropertySchema helper.
func compileJSONSchema(raw json.RawMessage) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse JSON Schema: %w", err)
	}
	const resource = "mem://schemapack/property-schema-check.json"
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(resource, doc); err != nil {
		return fmt.Errorf("add schema resource: %w", err)
	}
	if _, err := c.Compile(resource); err != nil {
		return fmt.Errorf("compile JSON Schema: %w", err)
	}
	return nil
}

// mustOSDirFS returns an [fs.FS] rooted at dir. It panics if dir is the
// empty string (programming error). This is a thin wrapper so ParseDir can
// call ParseFS without importing os directly in the main source file.
func mustOSDirFS(dir string) fs.FS {
	if dir == "" {
		panic("schemapack: mustOSDirFS: empty dir")
	}
	return os.DirFS(dir)
}
