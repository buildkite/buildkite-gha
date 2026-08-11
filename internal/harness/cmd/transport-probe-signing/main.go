package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/transport"
)

const keyID = "transport-probe-disposable-key"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 1 || (args[0] != "sign" && args[0] != "verify") {
		return fmt.Errorf("usage: transport-probe-signing sign|verify")
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	payload = []byte(strings.TrimSpace(string(payload)))
	key, err := disposableProbeKey()
	if err != nil {
		return err
	}
	if args[0] == "sign" {
		encoded, err := transport.SignProbePayload(key, payload)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(stdout, encoded)
		return err
	}
	verified, err := transport.VerifyProbePayload(key, string(payload))
	if err != nil {
		return err
	}
	_, err = stdout.Write(verified)
	return err
}

// This key is deliberately reproducible and public. It proves transport
// mechanics on queues with no protected capabilities; it grants no authority.
func disposableProbeKey() (transport.ES256Key, error) {
	curve := elliptic.P256()
	digest := sha256.Sum256([]byte("buildkite-gha transport probe disposable key"))
	orderMinusOne := new(big.Int).Sub(curve.Params().N, big.NewInt(1))
	scalar := new(big.Int).SetBytes(digest[:])
	scalar.Mod(scalar, orderMinusOne)
	scalar.Add(scalar, big.NewInt(1))
	private, err := ecdsa.ParseRawPrivateKey(curve, scalar.FillBytes(make([]byte, 32)))
	if err != nil {
		return transport.ES256Key{}, err
	}
	return transport.ES256Key{ID: keyID, Private: private, Public: &private.PublicKey}, nil
}
