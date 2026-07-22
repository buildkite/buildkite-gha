package main

import (
	"bytes"
	"encoding/json"
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
	var envelope map[string]string
	if err := json.Unmarshal(encoded.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	signature := envelope["signature"]
	if signature[0] == 'A' {
		envelope["signature"] = "B" + signature[1:]
	} else {
		envelope["signature"] = "A" + signature[1:]
	}
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	err = run([]string{"verify"}, bytes.NewReader(tampered), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "invalid ES256 signature") {
		t.Fatalf("verify error = %v, want invalid ES256 signature", err)
	}
}
