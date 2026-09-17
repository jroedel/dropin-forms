// Package forms holds the live form definitions, embedded into the binary.
//
// Embedded rather than read from disk, so that one file is the whole
// deployable artefact and a definition cannot drift out of step with the
// binary serving it. That matters more here than it usually would: a form's
// version is a fingerprint of its content, and a submission grant is signed
// over that fingerprint -- so a definition file updated on the server without
// a matching binary would invalidate every grant in flight for no reason
// anybody could see.
//
// These are live configuration, not documentation, which is why they are here
// rather than under docs/. The reasoning about them is in
// docs/design/drop-in-forms.md.
package forms

import "embed"

// FS is every definition. Read it with formtoml.Load.
//
//go:embed *.toml
var FS embed.FS
