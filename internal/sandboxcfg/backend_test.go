package sandboxcfg

import "testing"

func TestExtractBackend(t *testing.T) {
	for _, name := range []string{"cloud-hypervisor", "firecracker"} {
		input := map[string]string{NsBackend: ` { "name": "` + name + `" } `, "user": "kept"}
		got, cleaned, err := ExtractBackend(input, "")
		if err != nil || got != name || cleaned["user"] != "kept" {
			t.Fatalf("ExtractBackend = %q, %v, %v", got, cleaned, err)
		}
		if _, present := cleaned[NsBackend]; present {
			t.Fatal("selector retained")
		}
		cleaned["user"] = "changed"
		if input["user"] != "kept" || input[NsBackend] == "" {
			t.Fatal("extraction mutated caller metadata")
		}
	}
	for _, typed := range []string{"", "cloud-hypervisor", "firecracker"} {
		got, _, err := ExtractBackend(nil, typed)
		if err != nil || got != typed {
			t.Fatalf("normalized selection changed: %q, %v", got, err)
		}
	}
}

func TestExtractBackendRejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		``, `null`, `"firecracker"`, `true`, `1`, `[]`, `{}`, `{"name":""}`,
		`{"name":null}`, `{"name":true}`, `{"name":1}`, `{"name":{}}`, `{"name":[]}`,
		`{"name":"qemu"}`, `{"name":"Firecracker"}`, `{"name":" firecracker"}`, `{"name":"firecracker "}`,
		`{"Name":"firecracker"}`, `{"name":"firecracker","extra":1}`,
		`{"name":"firecracker","name":"cloud-hypervisor"}`, `{"name":"firecracker"} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := ExtractBackend(map[string]string{NsBackend: raw}, ""); err == nil {
				t.Fatal("invalid selector accepted")
			}
		})
	}
	for _, input := range []struct {
		meta  map[string]string
		typed string
	}{
		{nil, "qemu"},
		{map[string]string{InternalBackend: ""}, ""},
		{map[string]string{InternalBackend: "firecracker"}, ""},
		{map[string]string{NsBackend: `{"name":"firecracker"}`}, "firecracker"},
		{map[string]string{NsBackend: `{"name":"firecracker"}`}, "cloud-hypervisor"},
	} {
		if _, _, err := ExtractBackend(input.meta, input.typed); err == nil {
			t.Fatalf("ambiguous or protected selector accepted: %+v", input)
		}
	}
}

func TestBackendIsNotInheritedFromDefaults(t *testing.T) {
	got, err := MergeCreateMetadata(map[string]string{NsBackend: `{"name":"firecracker"}`, InternalBackend: "firecracker"}, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("inherited backend defaults: %v, %v", got, err)
	}
}
