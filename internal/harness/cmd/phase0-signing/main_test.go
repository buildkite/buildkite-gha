package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRoundTripsProbePayload(t *testing.T) {
	payload := `{"phase":"expected"}`
	var encoded bytes.Buffer
	if err := run([]string{"sign"}, strings.NewReader(payload), &encoded); err != nil {
		t.Fatal(err)
	}
	var verified bytes.Buffer
	if err := run([]string{"verify"}, strings.NewReader(encoded.String()), &verified); err != nil {
		t.Fatal(err)
	}
	if verified.String() != payload {
		t.Fatalf("verified payload = %q, want %q", verified.String(), payload)
	}
}

func TestRunRejectsTamperedProbePayload(t *testing.T) {
	var encoded bytes.Buffer
	if err := run([]string{"sign"}, strings.NewReader(`{"phase":"expected"}`), &encoded); err != nil {
		t.Fatal(err)
	}
	tampered := encoded.Bytes()
	tampered[len(tampered)-3] ^= 1
	if err := run([]string{"verify"}, bytes.NewReader(tampered), &bytes.Buffer{}); err == nil {
		t.Fatal("verify accepted a tampered envelope")
	}
}
