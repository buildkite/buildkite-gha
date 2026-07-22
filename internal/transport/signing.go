package transport

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
)

const markerType = "buildkite-gha-upload-marker+jws"

type protectedHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

type signedValue struct {
	Payload   string `json:"payload"`
	Protected string `json:"protected"`
	Signature string `json:"signature"`
}

// ES256Key is an in-memory Phase 0 signer/verifier. Production signing remains
// behind KMS; local tests use generated disposable keys.
type ES256Key struct {
	ID      string
	Private *ecdsa.PrivateKey
	Public  *ecdsa.PublicKey
}

func NewTestES256Key(id string) (ES256Key, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ES256Key{}, err
	}
	return ES256Key{ID: id, Private: private, Public: &private.PublicKey}, nil
}

func (k ES256Key) sign(typ string, payload []byte) (string, error) {
	if k.Private == nil || k.ID == "" {
		return "", errors.New("ES256 private key and key ID are required")
	}
	header, _ := json.Marshal(protectedHeader{Algorithm: "ES256", KeyID: k.ID, Type: typ})
	protected := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	hash := sha256.Sum256([]byte(protected + "." + encodedPayload))
	r, s, err := ecdsa.Sign(rand.Reader, k.Private, hash[:])
	if err != nil {
		return "", err
	}
	signature := append(padded(r, 32), padded(s, 32)...)
	value, _ := json.Marshal(signedValue{Payload: encodedPayload, Protected: protected, Signature: base64.RawURLEncoding.EncodeToString(signature)})
	return string(value), nil
}

func (k ES256Key) verify(typ, encoded string) ([]byte, error) {
	if k.Public == nil || k.ID == "" {
		return nil, errors.New("ES256 public key and key ID are required")
	}
	var value signedValue
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode signed value: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	canonicalValue, _ := json.Marshal(value)
	if canonicalValue == nil || !bytes.Equal(canonicalValue, []byte(encoded)) {
		return nil, errors.New("non-canonical signed value")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(value.Protected)
	if err != nil {
		return nil, errors.New("decode protected header")
	}
	var header protectedHeader
	headerDecoder := json.NewDecoder(bytes.NewReader(headerBytes))
	headerDecoder.DisallowUnknownFields()
	if err := headerDecoder.Decode(&header); err != nil || header.Algorithm != "ES256" || header.KeyID != k.ID || header.Type != typ {
		return nil, errors.New("untrusted protected header")
	}
	if err := requireJSONEOF(headerDecoder); err != nil {
		return nil, errors.New("untrusted protected header")
	}
	canonicalHeader, _ := json.Marshal(header)
	if !bytes.Equal(canonicalHeader, headerBytes) {
		return nil, errors.New("non-canonical protected header")
	}
	payload, err := base64.RawURLEncoding.DecodeString(value.Payload)
	if err != nil {
		return nil, errors.New("decode signed payload")
	}
	signature, err := base64.RawURLEncoding.DecodeString(value.Signature)
	if err != nil || len(signature) != 64 {
		return nil, errors.New("invalid ES256 signature encoding")
	}
	hash := sha256.Sum256([]byte(value.Protected + "." + value.Payload))
	if !ecdsa.Verify(k.Public, hash[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		return nil, errors.New("invalid ES256 signature")
	}
	return payload, nil
}

func padded(value *big.Int, size int) []byte {
	out := make([]byte, size)
	value.FillBytes(out)
	return out
}

// UploadJob binds each deterministic key to the signed plan digest embedded in
// that job's signed pipeline environment.
type UploadJob struct {
	Key        string `json:"key"`
	PlanDigest string `json:"plan_digest"`
}

// UploadIntent is immutable retry state for one importer/build pair.
type UploadIntent struct {
	BuildID        string      `json:"build_id"`
	ImporterKey    string      `json:"importer_key"`
	PipelineDigest string      `json:"pipeline_digest"`
	Jobs           []UploadJob `json:"jobs"`
}

type uploadMarker struct {
	Phase  string       `json:"phase"`
	Intent UploadIntent `json:"intent"`
}

// MarkerValue is ready for Buildkite metadata publication.
type MarkerValue struct {
	key            string
	value          string
	phase          string
	pipelineDigest string
	importerKey    string
	jobs           []UploadJob
}

// Encoded returns the signed metadata value for retry classification.
func (m MarkerValue) Encoded() string { return m.value }

func normalizeIntent(intent UploadIntent) (UploadIntent, error) {
	if !uuidPattern.MatchString(intent.BuildID) || !keyPattern.MatchString(intent.ImporterKey) || !digestPattern.MatchString(intent.PipelineDigest) {
		return UploadIntent{}, errors.New("invalid upload intent identity")
	}
	intent.Jobs = append([]UploadJob(nil), intent.Jobs...)
	if len(intent.Jobs) != 2 {
		return UploadIntent{}, fmt.Errorf("Phase 0 upload intent requires exactly two jobs, got %d", len(intent.Jobs))
	}
	sort.Slice(intent.Jobs, func(i, j int) bool { return intent.Jobs[i].Key < intent.Jobs[j].Key })
	for i, job := range intent.Jobs {
		if !keyPattern.MatchString(job.Key) || !digestPattern.MatchString(job.PlanDigest) {
			return UploadIntent{}, errors.New("invalid upload job identity")
		}
		if i > 0 && intent.Jobs[i-1].Key == job.Key {
			return UploadIntent{}, fmt.Errorf("duplicate upload job %q", job.Key)
		}
	}
	return intent, nil
}

// SignMarker produces either the expected or completed signed metadata value.
func SignMarker(key ES256Key, intent UploadIntent, phase string) (MarkerValue, error) {
	if phase != "expected" && phase != "completed" {
		return MarkerValue{}, fmt.Errorf("invalid marker phase %q", phase)
	}
	intent, err := normalizeIntent(intent)
	if err != nil {
		return MarkerValue{}, err
	}
	payload, _ := json.Marshal(uploadMarker{Phase: phase, Intent: intent})
	value, err := key.sign(markerType, payload)
	if err != nil {
		return MarkerValue{}, err
	}
	return MarkerValue{
		key:            fmt.Sprintf("buildkite-gha/v1/uploads/%s/%s", intent.ImporterKey, phase),
		value:          value,
		phase:          phase,
		pipelineDigest: intent.PipelineDigest,
		importerKey:    intent.ImporterKey,
		jobs:           append([]UploadJob(nil), intent.Jobs...),
	}, nil
}

func verifyMarker(key ES256Key, encoded, phase string) (UploadIntent, error) {
	payload, err := key.verify(markerType, encoded)
	if err != nil {
		return UploadIntent{}, err
	}
	var marker uploadMarker
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil || marker.Phase != phase {
		return UploadIntent{}, errors.New("invalid signed marker payload")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return UploadIntent{}, errors.New("invalid signed marker payload")
	}
	normalized, err := normalizeIntent(marker.Intent)
	if err != nil {
		return UploadIntent{}, err
	}
	canonical, _ := json.Marshal(uploadMarker{Phase: phase, Intent: normalized})
	if !bytes.Equal(canonical, payload) {
		return UploadIntent{}, errors.New("non-canonical signed marker payload")
	}
	return normalized, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing JSON data")
	}
	return nil
}
