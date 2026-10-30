// Package ontology hosts the M1 schema set: the Go structs that define
// Mazarbul's atomic memory units (Episode, Entity, Mention, Relation) and
// the JSON Schemas generated from them via invopop/jsonschema.
//
// One source of truth per kind: the Go struct. The wire-shaped JSON
// Schema is reflected from the struct at first access through a
// sync.OnceValue cache, then registered with kernel.Ontology via each
// kind's exported Register function. Tests inject a fake Ontology; the
// real registry (#37) replaces NoopOntology in production wiring.
//
// AIP naming applies throughout: createTime (not createdAt), resource
// names of the form "<collection>/<uid>" (plural, lowercase), camelCase
// JSON field names, ULID identifiers.
//
// Episode (#22), Entity (#34), and Relation (#36) ship in M1; Mention
// (#35) follows the same pattern.
package ontology
