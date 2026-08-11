package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"
)

const (
	bindingType = "buildkite-gha-runtime-binding+jws"
	// RuntimeBindingIssuer is the plan-envelope issuer shared with plan envelopes.
	RuntimeBindingIssuer = "buildkite-gha-plan-envelope"
	maxBindingLifetime   = int64((24 * time.Hour) / time.Second)
)

// RuntimeBinding is the signed transport integrity binding and is inert until its
// signature has been verified. It does not authorize protected capabilities.
type RuntimeBinding struct {
	Issuer            string       `json:"iss"`
	ID                string       `json:"jti"`
	IssuedAt          int64        `json:"iat"`
	ExpiresAt         int64        `json:"exp"`
	Build             BuildBinding `json:"build"`
	StepKey           string       `json:"step_key"`
	Queue             string       `json:"queue"`
	Event             EventBinding `json:"event"`
	PlanDigest        string       `json:"plan_digest"`
	CapabilityCeiling []string     `json:"capability_ceiling"`
}

type BuildBinding struct {
	OrganizationID string `json:"organization_id"`
	PipelineID     string `json:"pipeline_id"`
	BuildID        string `json:"build_id"`
}

type EventBinding struct {
	Provider    string `json:"provider"`
	Name        string `json:"name"`
	Repository  string `json:"repository"`
	Ref         string `json:"ref"`
	Commit      string `json:"commit"`
	Digest      string `json:"digest"`
	Trust       string `json:"trust"`
	Attestation string `json:"attestation"`
}

type ObservedRuntime struct {
	Now                    time.Time
	BindingID              string
	Build                  BuildBinding
	StepKey                string
	Queue                  string
	Event                  EventBinding
	PlanDigest             string
	RequiredCapabilities   []string
	LocalCapabilityCeiling []string
}

type VerificationError struct {
	Code string
}

func (e VerificationError) Error() string { return e.Code }

func normalizeBinding(binding RuntimeBinding) (RuntimeBinding, error) {
	if binding.Issuer != RuntimeBindingIssuer || !uuidPattern.MatchString(binding.ID) || binding.IssuedAt < 0 || binding.ExpiresAt <= binding.IssuedAt || binding.ExpiresAt-binding.IssuedAt > maxBindingLifetime {
		return RuntimeBinding{}, fmt.Errorf("invalid binding lifetime or replay identity")
	}
	if !uuidPattern.MatchString(binding.Build.OrganizationID) || !uuidPattern.MatchString(binding.Build.PipelineID) || !uuidPattern.MatchString(binding.Build.BuildID) {
		return RuntimeBinding{}, fmt.Errorf("missing build binding")
	}
	if !keyPattern.MatchString(binding.StepKey) || !keyPattern.MatchString(binding.Queue) || !digestPattern.MatchString(binding.PlanDigest) || !digestPattern.MatchString(binding.Event.Digest) {
		return RuntimeBinding{}, fmt.Errorf("invalid runtime binding")
	}
	if (binding.Event.Provider != "github" && binding.Event.Provider != "cursor-origin") || binding.Event.Name == "" || binding.Event.Repository == "" || binding.Event.Ref == "" || !commitPattern.MatchString(binding.Event.Commit) ||
		(binding.Event.Trust != "trusted" && binding.Event.Trust != "untrusted") ||
		(binding.Event.Attestation != "buildkite-webhook" && binding.Event.Attestation != "provider-signed" && binding.Event.Attestation != "manual-unattested") {
		return RuntimeBinding{}, fmt.Errorf("missing event binding")
	}
	binding.CapabilityCeiling = append([]string(nil), binding.CapabilityCeiling...)
	sort.Strings(binding.CapabilityCeiling)
	for i, capability := range binding.CapabilityCeiling {
		if !validCapability(capability) || (i > 0 && binding.CapabilityCeiling[i-1] == capability) {
			return RuntimeBinding{}, fmt.Errorf("invalid capability ceiling")
		}
	}
	return binding, nil
}

func SignRuntimeBinding(key ES256Key, binding RuntimeBinding) (string, error) {
	binding, err := normalizeBinding(binding)
	if err != nil {
		return "", err
	}
	payload, err := canonicalJSON(binding)
	if err != nil {
		return "", fmt.Errorf("canonicalize runtime binding: %w", err)
	}
	return key.sign(bindingType, payload)
}

// VerifyRuntimeBinding checks signature authority before comparing any
// attacker-controlled claim with immutable Buildkite and event context.
func VerifyRuntimeBinding(key ES256Key, encoded string, observed ObservedRuntime) (RuntimeBinding, error) {
	payload, err := key.verify(bindingType, encoded)
	if err != nil {
		return RuntimeBinding{}, VerificationError{Code: "E_SIGNATURE"}
	}
	var binding RuntimeBinding
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		return RuntimeBinding{}, VerificationError{Code: "E_SCHEMA"}
	}
	if err := requireJSONEOF(decoder); err != nil {
		return RuntimeBinding{}, VerificationError{Code: "E_SCHEMA"}
	}
	binding, err = normalizeBinding(binding)
	if err != nil {
		return RuntimeBinding{}, VerificationError{Code: "E_SCHEMA"}
	}
	canonical, err := canonicalJSON(binding)
	if err != nil {
		return RuntimeBinding{}, VerificationError{Code: "E_SCHEMA"}
	}
	if !bytes.Equal(canonical, payload) {
		return RuntimeBinding{}, VerificationError{Code: "E_SCHEMA"}
	}
	if observed.Now.IsZero() || binding.IssuedAt > observed.Now.Unix() || observed.Now.Unix() >= binding.ExpiresAt {
		return RuntimeBinding{}, VerificationError{Code: "E_EXPIRED"}
	}
	if binding.ID != observed.BindingID {
		return RuntimeBinding{}, VerificationError{Code: "E_REPLAY_BINDING"}
	}
	if binding.Build != observed.Build {
		return RuntimeBinding{}, VerificationError{Code: "E_BUILD_BINDING"}
	}
	if binding.StepKey != observed.StepKey {
		return RuntimeBinding{}, VerificationError{Code: "E_STEP_BINDING"}
	}
	if binding.Queue != observed.Queue {
		return RuntimeBinding{}, VerificationError{Code: "E_QUEUE_BINDING"}
	}
	if binding.Event != observed.Event {
		return RuntimeBinding{}, VerificationError{Code: "E_EVENT_BINDING"}
	}
	if binding.PlanDigest != observed.PlanDigest {
		return RuntimeBinding{}, VerificationError{Code: "E_PLAN_BINDING"}
	}
	for _, capability := range binding.CapabilityCeiling {
		if !slices.Contains(observed.LocalCapabilityCeiling, capability) {
			return RuntimeBinding{}, VerificationError{Code: "E_CAPABILITY_POLICY"}
		}
	}
	for _, capability := range observed.RequiredCapabilities {
		if !validCapability(capability) || !slices.Contains(binding.CapabilityCeiling, capability) || !slices.Contains(observed.LocalCapabilityCeiling, capability) {
			return RuntimeBinding{}, VerificationError{Code: "E_CAPABILITY_BINDING"}
		}
	}
	return binding, nil
}

func validCapability(capability string) bool {
	switch capability {
	case "network", "docker", "privileged-container", "secrets", "provider-token-read", "provider-token-write":
		return true
	default:
		return false
	}
}
