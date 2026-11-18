package sandboxcfg

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kuasar-sandbox/orchestrator/internal/strictjson"
)

// NsBackend is a Create-only selector, not a portable template default or a
// mutable sandbox property. InternalBackend is the core-owned durable selection.
const (
	NsBackend       = "kuasar-sandbox.backend"
	InternalBackend = "kuasar-sandbox.internal.backend"
)

// RejectObsoleteBackend rejects even empty/null legacy selectors rather than
// silently ignoring them in otherwise extensible public Create envelopes.
func RejectObsoleteBackend(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key := range fields {
		if strings.EqualFold(key, "backend") {
			return fmt.Errorf("top-level backend is obsolete; use metadata[%q] with {\"name\":\"firecracker\"} or {\"name\":\"cloud-hypervisor\"}", NsBackend)
		}
	}
	return nil
}

// ExtractBackend validates and consumes public metadata without mutating it.
// The typed selector is for already-normalized internal requests; supplying both
// carriers is rejected so there is never a second authority or precedence rule.
func ExtractBackend(meta map[string]string, backend string) (string, map[string]string, error) {
	if _, present := meta[InternalBackend]; present {
		return "", nil, fmt.Errorf("sandbox backend is node-managed (%s)", InternalBackend)
	}
	if backend != "" && backend != "cloud-hypervisor" && backend != "firecracker" {
		return "", nil, fmt.Errorf("unsupported sandbox backend %q", backend)
	}
	raw, present := meta[NsBackend]
	if !present {
		return backend, meta, nil
	}
	if backend != "" {
		return "", nil, fmt.Errorf("%s conflicts with normalized Backend", NsBackend)
	}
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", nil, fmt.Errorf("sandboxcfg: metadata[%q] must be a JSON object", NsBackend)
	}
	var fields map[string]json.RawMessage
	if err := strictjson.Decode([]byte(trimmed), &fields); err != nil {
		return "", nil, fmt.Errorf("sandboxcfg: metadata[%q]: %w", NsBackend, err)
	}
	for key := range fields {
		if key != "name" {
			return "", nil, fmt.Errorf("sandboxcfg: metadata[%q] contains unknown field %q", NsBackend, key)
		}
	}
	if err := json.Unmarshal(fields["name"], &backend); err != nil || (backend != "cloud-hypervisor" && backend != "firecracker") {
		return "", nil, fmt.Errorf("sandboxcfg: metadata[%q].name must be cloud-hypervisor or firecracker", NsBackend)
	}
	cleaned := make(map[string]string, len(meta)-1)
	for key, value := range meta {
		if key != NsBackend {
			cleaned[key] = value
		}
	}
	return backend, cleaned, nil
}
