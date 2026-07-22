package transport

import (
	"testing"
	"time"
)

const (
	testBindingID = "66666666-6666-4666-8666-666666666666"
	testIssuedAt  = int64(1784682000)
)

func TestVerifyRuntimeBindingChecksAllSignedTrustInputs(t *testing.T) {
	key := testKey(t)
	binding := RuntimeBinding{
		Issuer:            RuntimeBindingIssuer,
		ID:                testBindingID,
		IssuedAt:          testIssuedAt,
		ExpiresAt:         testIssuedAt + 3600,
		Build:             BuildBinding{OrganizationID: "11111111-1111-4111-8111-111111111111", PipelineID: "22222222-2222-4222-8222-222222222222", BuildID: "33333333-3333-4333-8333-333333333333"},
		StepKey:           "gha-producer",
		Queue:             "gha-runtime",
		Event:             EventBinding{Provider: "github", Name: "pull_request", Repository: "https://github.com/acme/widgets", Ref: "refs/pull/1/head", Commit: "0123456789abcdef0123456789abcdef01234567", Digest: Digest([]byte("event")), Trust: "untrusted", Attestation: "buildkite-webhook"},
		PlanDigest:        Digest([]byte("plan")),
		CapabilityCeiling: []string{"secrets", "network"},
	}
	encoded, err := SignRuntimeBinding(key, binding)
	if err != nil {
		t.Fatal(err)
	}
	observed := ObservedRuntime{
		Now: time.Unix(testIssuedAt+1, 0), BindingID: binding.ID,
		Build: binding.Build, StepKey: binding.StepKey, Queue: binding.Queue, Event: binding.Event,
		PlanDigest: binding.PlanDigest, RequiredCapabilities: []string{"network"}, LocalCapabilityCeiling: []string{"network", "secrets"},
	}
	verified, err := VerifyRuntimeBinding(key, encoded, observed)
	if err != nil {
		t.Fatal(err)
	}
	if verified.CapabilityCeiling[0] != "network" {
		t.Fatalf("capabilities were not canonicalized: %#v", verified.CapabilityCeiling)
	}

	tests := []struct {
		name string
		edit func(*ObservedRuntime)
		code string
	}{
		{name: "build", edit: func(o *ObservedRuntime) { o.Build.BuildID = "44444444-4444-4444-8444-444444444444" }, code: "E_BUILD_BINDING"},
		{name: "step", edit: func(o *ObservedRuntime) { o.StepKey = "gha-consumer" }, code: "E_STEP_BINDING"},
		{name: "queue", edit: func(o *ObservedRuntime) { o.Queue = "gha-privileged" }, code: "E_QUEUE_BINDING"},
		{name: "event", edit: func(o *ObservedRuntime) { o.Event.Trust = "trusted" }, code: "E_EVENT_BINDING"},
		{name: "plan", edit: func(o *ObservedRuntime) { o.PlanDigest = Digest([]byte("other")) }, code: "E_PLAN_BINDING"},
		{name: "replay identity", edit: func(o *ObservedRuntime) { o.BindingID = "77777777-7777-4777-8777-777777777777" }, code: "E_REPLAY_BINDING"},
		{name: "expired", edit: func(o *ObservedRuntime) { o.Now = time.Unix(binding.ExpiresAt, 0) }, code: "E_EXPIRED"},
		{name: "not yet valid", edit: func(o *ObservedRuntime) { o.Now = time.Unix(binding.IssuedAt-1, 0) }, code: "E_EXPIRED"},
		{name: "capability", edit: func(o *ObservedRuntime) { o.RequiredCapabilities = []string{"privileged-container"} }, code: "E_CAPABILITY_BINDING"},
		{name: "local policy", edit: func(o *ObservedRuntime) { o.LocalCapabilityCeiling = []string{"network"} }, code: "E_CAPABILITY_POLICY"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := observed
			test.edit(&changed)
			_, err := VerifyRuntimeBinding(key, encoded, changed)
			if err == nil || err.Error() != test.code {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestVerifyRuntimeBindingRejectsTamperingBeforeClaims(t *testing.T) {
	key := testKey(t)
	binding := RuntimeBinding{
		Issuer: RuntimeBindingIssuer, ID: testBindingID, IssuedAt: testIssuedAt, ExpiresAt: testIssuedAt + 3600,
		Build:   BuildBinding{OrganizationID: "11111111-1111-4111-8111-111111111111", PipelineID: "22222222-2222-4222-8222-222222222222", BuildID: "33333333-3333-4333-8333-333333333333"},
		StepKey: "step", Queue: "queue", Event: EventBinding{Provider: "github", Name: "push", Repository: "https://github.com/acme/widgets", Ref: "refs/heads/main", Commit: "0123456789abcdef0123456789abcdef01234567", Digest: Digest([]byte("event")), Trust: "trusted", Attestation: "buildkite-webhook"},
		PlanDigest: Digest([]byte("plan")),
	}
	encoded, err := SignRuntimeBinding(key, binding)
	if err != nil {
		t.Fatal(err)
	}
	encoded = encoded[:len(encoded)-1] + "x"
	_, err = VerifyRuntimeBinding(key, encoded, ObservedRuntime{})
	if err == nil || err.Error() != "E_SIGNATURE" {
		t.Fatalf("error = %v, want E_SIGNATURE", err)
	}
}

func TestRuntimeBindingRejectsInvalidLifetime(t *testing.T) {
	key := testKey(t)
	binding := RuntimeBinding{
		Issuer: RuntimeBindingIssuer, ID: testBindingID, IssuedAt: testIssuedAt, ExpiresAt: testIssuedAt + 86401,
		Build:   BuildBinding{OrganizationID: "11111111-1111-4111-8111-111111111111", PipelineID: "22222222-2222-4222-8222-222222222222", BuildID: "33333333-3333-4333-8333-333333333333"},
		StepKey: "step", Queue: "queue", Event: EventBinding{Provider: "github", Name: "push", Repository: "https://github.com/acme/widgets", Ref: "refs/heads/main", Commit: "0123456789abcdef0123456789abcdef01234567", Digest: Digest([]byte("event")), Trust: "trusted", Attestation: "buildkite-webhook"},
		PlanDigest: Digest([]byte("plan")),
	}
	if _, err := SignRuntimeBinding(key, binding); err == nil {
		t.Fatal("binding longer than 24 hours was signed")
	}
	binding.ExpiresAt = binding.IssuedAt
	if _, err := SignRuntimeBinding(key, binding); err == nil {
		t.Fatal("empty binding lifetime was signed")
	}
}
