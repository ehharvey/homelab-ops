// Package seedwire is the JSON shape of POST /instances/{name}/seed's answer
// (#242): one string field per rendered seed file, so each file stays
// independently addressable (curl | jq) rather than bundled into one
// multi-document blob. internal/server encodes it and internal/apiclient
// decodes it, both through Fields, so a seed file is named in one place.
package seedwire

import (
	"encoding/json"
	"fmt"

	"github.com/ehharvey/homelab-ops/internal/hostos"
)

// Field is one seed file's JSON field.
type Field struct {
	JSON string // the field's name in the answer
	File string // the file it holds, named as hostos.RenderConfig names it
}

// Fields are the answer's fields, in RenderConfig's order. They're IncusOS's
// seed files, the only backend's output; a second backend's files need their
// own shape, decided with that backend.
var Fields = []Field{
	{JSON: "install_yaml", File: "install.yaml"},
	{JSON: "network_yaml", File: "network.yaml"},
	{JSON: "applications_yaml", File: "applications.yaml"},
	{JSON: "incus_yaml", File: "incus.yaml"},
}

// Encode maps rendered files onto Fields, keyed by JSON name. Every field
// must be filled exactly once and no file left over, so a renamed or missing
// file is an error rather than an empty field in a 200.
func Encode(files []hostos.File) (map[string]string, error) {
	byFile := make(map[string]string, len(Fields))
	for _, f := range Fields {
		byFile[f.File] = f.JSON
	}
	resp := make(map[string]string, len(Fields))
	for _, f := range files {
		field, ok := byFile[f.Name]
		if !ok {
			return nil, fmt.Errorf("rendered file %q has no field in the seed response", f.Name)
		}
		if _, dup := resp[field]; dup {
			return nil, fmt.Errorf("rendered seed has %s twice", f.Name)
		}
		resp[field] = string(f.Data)
	}
	for _, f := range Fields {
		if _, ok := resp[f.JSON]; !ok {
			return nil, fmt.Errorf("rendered seed has no %s", f.File)
		}
	}
	return resp, nil
}

// Decode turns an answer back into the files it holds, in Fields' order. A
// field the answer lacks, or leaves empty, is an error; a field Fields
// doesn't name is ignored.
func Decode(body []byte) ([]hostos.File, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("decode seed: %w", err)
	}
	files := make([]hostos.File, 0, len(Fields))
	for _, f := range Fields {
		var v string
		if raw, ok := fields[f.JSON]; ok {
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("decode seed: %s: %w", f.JSON, err)
			}
		}
		if v == "" {
			return nil, fmt.Errorf("decode seed: the web app's answer has no %s (field %q)", f.File, f.JSON)
		}
		files = append(files, hostos.File{Name: f.File, Data: []byte(v)})
	}
	return files, nil
}
