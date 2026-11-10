// Package seedwire is the JSON shape of POST /instances/{name}/seed's answer
// (#242): one string field per rendered seed file, so each file stays
// independently addressable (curl | jq) rather than bundled into one
// multi-document blob. internal/server encodes it and the CLI's fetchSeed
// decodes it, both through Fields, so a seed file is named in one place.
package seedwire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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

// Answer is the seed route's body, keyed by JSON name. It marshals in Fields'
// order, so the route prints the files in RenderConfig's order.
type Answer map[string]string

// MarshalJSON implements json.Marshaler.
func (a Answer) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range Fields {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(f.JSON)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(a[f.JSON])
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Encode maps rendered files onto Fields, keyed by JSON name. Every field
// must be filled exactly once and no file left over, so a renamed or missing
// file is an error rather than an empty field in a 200.
func Encode(files []hostos.File) (Answer, error) {
	byFile := make(map[string]string, len(Fields))
	for _, f := range Fields {
		byFile[f.File] = f.JSON
	}
	resp := make(Answer, len(Fields))
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
// field the answer lacks, or leaves empty, is an error. So is a field Fields
// doesn't name: every field is a seed file, so a web app newer than this CLI
// would otherwise lose one from the install media without a word.
func Decode(body []byte) ([]hostos.File, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("decode seed: %w", err)
	}
	known := make(map[string]bool, len(Fields))
	for _, f := range Fields {
		known[f.JSON] = true
	}
	var unknown []string
	for name := range fields {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("decode seed: the web app's answer has fields this bootstrap doesn't know (%s), so install media built from it would be incomplete; update bootstrap to match the web app", strings.Join(unknown, ", "))
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
