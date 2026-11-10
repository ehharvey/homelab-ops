package seedwire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/hostos"
)

var four = []hostos.File{
	{Name: "install.yaml", Data: []byte("i\n")},
	{Name: "network.yaml", Data: []byte("n\n")},
	{Name: "applications.yaml", Data: []byte("a\n")},
	{Name: "incus.yaml", Data: []byte("c\n")},
}

// TestEncodeDecodeRoundTrip: Decode turns Encode's answer, as JSON, back into
// the same files in the same order, under the wire names the route has
// always used.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	resp, err := Encode(four)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := map[string]string{"install_yaml": "i\n", "network_yaml": "n\n", "applications_yaml": "a\n", "incus_yaml": "c\n"}
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("Encode = %v, want %v", resp, want)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, four) {
		t.Errorf("Decode(Encode(files)) = %v, want %v", got, four)
	}
}

// TestEncodeNeedsExactlyTheFourFiles pins that the seed route can't answer
// 200 with an empty field: a missing, unexpected or repeated rendered file is
// an error, not a blank YAML string.
func TestEncodeNeedsExactlyTheFourFiles(t *testing.T) {
	renamed := append([]hostos.File(nil), four...)
	renamed[2].Name = "applications.yml"
	cases := map[string]struct {
		files []hostos.File
		want  string
	}{
		"missing": {four[:3], "no incus.yaml"},
		"renamed": {renamed, `"applications.yml" has no field`},
		"twice":   {append(append([]hostos.File(nil), four...), four[0]), "install.yaml twice"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Encode(tc.files); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Encode error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestDecodeIgnoresUnknownFields(t *testing.T) {
	body := `{"install_yaml":"i\n","network_yaml":"n\n","applications_yaml":"a\n","incus_yaml":"c\n","later_field":{"x":1}}`
	got, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, four) {
		t.Errorf("Decode = %v, want %v", got, four)
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"not JSON":       {`not json`, "decode seed"},
		"missing field":  {`{"install_yaml":"i","network_yaml":"n","applications_yaml":"a"}`, "no incus.yaml"},
		"empty field":    {`{"install_yaml":"","network_yaml":"n","applications_yaml":"a","incus_yaml":"c"}`, "no install.yaml"},
		"field not text": {`{"install_yaml":1,"network_yaml":"n","applications_yaml":"a","incus_yaml":"c"}`, "install_yaml"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Decode error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
