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
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const markerType = "buildkite-gha-upload-marker+jws"

const probePayloadType = "buildkite-gha-phase0-live-probe+jws"

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

// SignProbePayload signs already-canonical JSON for the unprivileged Phase 0
// live transport probe. Production plan signing uses a separate trust path.
func SignProbePayload(key ES256Key, payload []byte) (string, error) {
	if !json.Valid(payload) {
		return "", errors.New("probe payload must be valid JSON")
	}
	return key.sign(probePayloadType, payload)
}

// VerifyProbePayload verifies and returns the Phase 0 live probe payload.
func VerifyProbePayload(key ES256Key, encoded string) ([]byte, error) {
	return key.verify(probePayloadType, encoded)
}

func (k ES256Key) sign(typ string, payload []byte) (string, error) {
	if k.Private == nil || k.ID == "" {
		return "", errors.New("ES256 private key and key ID are required")
	}
	header, err := canonicalJSON(protectedHeader{Algorithm: "ES256", KeyID: k.ID, Type: typ})
	if err != nil {
		return "", fmt.Errorf("canonicalize protected header: %w", err)
	}
	protected := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	hash := sha256.Sum256([]byte(protected + "." + encodedPayload))
	r, s, err := ecdsa.Sign(rand.Reader, k.Private, hash[:])
	if err != nil {
		return "", err
	}
	signature := append(padded(r, 32), padded(s, 32)...)
	value, err := canonicalJSON(signedValue{Payload: encodedPayload, Protected: protected, Signature: base64.RawURLEncoding.EncodeToString(signature)})
	if err != nil {
		return "", fmt.Errorf("canonicalize signed value: %w", err)
	}
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
	canonicalValue, err := canonicalJSON(value)
	if err != nil {
		return nil, errors.New("non-canonical signed value")
	}
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
	canonicalHeader, err := canonicalJSON(header)
	if err != nil {
		return nil, errors.New("non-canonical protected header")
	}
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

// canonicalJSON encodes the bounded, integer-only Phase 0 signing models using
// RFC 8785 object ordering and string escaping. The signed models deliberately
// exclude floating-point values, maps, and interface fields so their complete
// JCS domain remains small and auditable.
func canonicalJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	if err := appendCanonical(&out, reflect.ValueOf(value)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func appendCanonical(out *bytes.Buffer, value reflect.Value) error {
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return errors.New("nil value is outside the signed JSON domain")
		}
		return appendCanonical(out, value.Elem())
	}
	switch value.Kind() {
	case reflect.Struct:
		type field struct {
			name  string
			value reflect.Value
		}
		fields := make([]field, 0, value.NumField())
		model := value.Type()
		for i := range value.NumField() {
			definition := model.Field(i)
			name := definition.Tag.Get("json")
			if comma := strings.IndexByte(name, ','); comma >= 0 {
				name = name[:comma]
			}
			if name == "" || name == "-" || definition.PkgPath != "" {
				return fmt.Errorf("field %s has no signed JSON name", definition.Name)
			}
			fields = append(fields, field{name: name, value: value.Field(i)})
		}
		sort.Slice(fields, func(i, j int) bool { return lessUTF16(fields[i].name, fields[j].name) })
		out.WriteByte('{')
		for i, field := range fields {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonicalString(out, field.name); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := appendCanonical(out, field.value); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	case reflect.Slice, reflect.Array:
		out.WriteByte('[')
		for i := range value.Len() {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonical(out, value.Index(i)); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case reflect.String:
		return appendCanonicalString(out, value.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		out.WriteString(strconv.FormatInt(value.Int(), 10))
	default:
		return fmt.Errorf("%s is outside the signed JSON domain", value.Kind())
	}
	return nil
}

func appendCanonicalString(out *bytes.Buffer, value string) error {
	if !utf8.ValidString(value) {
		return errors.New("invalid UTF-8 string in signed JSON")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = bytes.ReplaceAll(encoded, []byte(`\u003c`), []byte("<"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u003e`), []byte(">"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u0026`), []byte("&"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u2028`), []byte("\u2028"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u2029`), []byte("\u2029"))
	out.Write(encoded)
	return nil
}

func lessUTF16(left, right string) bool {
	return slices.Compare(utf16.Encode([]rune(left)), utf16.Encode([]rune(right))) < 0
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
		return UploadIntent{}, fmt.Errorf("phase 0 upload intent requires exactly two jobs, got %d", len(intent.Jobs))
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
	payload, err := canonicalJSON(uploadMarker{Phase: phase, Intent: intent})
	if err != nil {
		return MarkerValue{}, fmt.Errorf("canonicalize upload marker: %w", err)
	}
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
	canonical, err := canonicalJSON(uploadMarker{Phase: phase, Intent: normalized})
	if err != nil {
		return UploadIntent{}, errors.New("invalid signed marker payload")
	}
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
