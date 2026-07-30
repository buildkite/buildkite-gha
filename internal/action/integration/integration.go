// Package integration classifies actions with Buildkite-specific execution or
// service requirements.
package integration

import (
	"fmt"
	"sort"
	"strings"
)

// Identity is the canonical, resolved portion of an action lock used for
// integration matching. The immutable commit and source digest remain runtime
// verification evidence rather than catalog selectors.
type Identity struct {
	Source     string
	Repository string
	Path       string
}

// Adapter identifies a runtime replacement for an action's upstream lifecycle.
type Adapter string

const (
	// AdapterCheckoutExactEventSHA checks out the event repository at its exact
	// event SHA without executing actions/checkout's JavaScript lifecycle.
	AdapterCheckoutExactEventSHA Adapter = "checkout-exact-event-sha-v1"
)

// Service identifies an Actions runtime service used by a known action.
// Service classification supports admission diagnostics only: services may be
// used transitively and must not be provisioned based solely on this catalog.
type Service string

const (
	ServiceArtifact Service = "artifact"
	ServiceCache    Service = "cache"
)

// Descriptor records Buildkite-specific handling for one exact action identity.
type Descriptor struct {
	Adapter Adapter
	Service Service
}

var catalog = map[Identity]Descriptor{
	{Source: "github", Repository: "actions/checkout"}:                       {Adapter: AdapterCheckoutExactEventSHA},
	{Source: "github", Repository: "actions/cache"}:                          {Service: ServiceCache},
	{Source: "github", Repository: "actions/cache", Path: "restore"}:         {Service: ServiceCache},
	{Source: "github", Repository: "actions/cache", Path: "save"}:            {Service: ServiceCache},
	{Source: "github", Repository: "actions/upload-artifact"}:                {Service: ServiceArtifact},
	{Source: "github", Repository: "actions/upload-artifact", Path: "merge"}: {Service: ServiceArtifact},
	{Source: "github", Repository: "actions/download-artifact"}:              {Service: ServiceArtifact},
}

// Lookup returns the integration for an exact canonical action identity.
func Lookup(identity Identity) (Descriptor, bool) {
	descriptor, ok := catalog[identity]
	return descriptor, ok
}

// ValidateCheckoutInputs enforces the input contract implemented by the
// tokenless exact-event-SHA checkout adapter.
func ValidateCheckoutInputs(inputs map[string]string, repository, sha string) error {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		value := inputs[name]
		normalized := strings.ToLower(name)
		if seen[normalized] {
			return fmt.Errorf("duplicate case-insensitive input %q is unsupported; Phase 6 is required", name)
		}
		seen[normalized] = true
		switch normalized {
		case "repository":
			if strings.EqualFold(value, repository) {
				continue
			}
		case "ref":
			if value == sha {
				continue
			}
		case "persist-credentials":
			if value == "false" {
				continue
			}
		case "fetch-depth":
			if value == "1" {
				continue
			}
		case "clean", "set-safe-directory":
			if value == "true" {
				continue
			}
		}
		return fmt.Errorf("explicit input %q is unsupported (including empty values); Phase 6 is required", name)
	}
	return nil
}
