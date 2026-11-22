// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

// Format is how a request writes its spec (ADR 0057): JSON, or YAML,
// which is read as the JSON it stands for. Nothing stores YAML: a
// deployment's spec is its normalised JSON.
type Format int

const (
	// FormatJSON: the spec is a JSON object (the zero value).
	FormatJSON Format = iota
	// FormatYAML: the spec is one YAML document.
	FormatYAML
)

// ParseAs reads raw in the format f, Parse or ParseYAML.
func ParseAs(raw []byte, f Format) (Spec, []Finding) {
	if f == FormatYAML {
		return ParseYAML(raw)
	}
	return Parse(raw)
}
