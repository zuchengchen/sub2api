package tlsfingerprint

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"fmt"

	utls "github.com/refraction-networking/utls"
)

// hybridKeyShare holds the private keys behind prefilled key shares.
type hybridKeyShare struct {
	x25519 *ecdh.PrivateKey
	mlkem  *mlkem.DecapsulationKey768
}

// prefillHybridKeyShares fills the X25519MLKEM768 and X25519 key shares of
// spec so the standalone X25519 share equals the classical half of the hybrid
// share (rustls behavior; see rustls crypto/aws_lc_rs/pq/hybrid.rs). uTLS
// skips generation for shares that already carry data, so the caller must
// install the returned keys into HandshakeState after ApplyPreset.
//
// Returns (nil, nil) when spec does not offer both groups.
func prefillHybridKeyShares(spec *utls.ClientHelloSpec) (*hybridKeyShare, error) {
	var ks *utls.KeyShareExtension
	for _, ext := range spec.Extensions {
		if e, ok := ext.(*utls.KeyShareExtension); ok {
			ks = e
			break
		}
	}
	if ks == nil {
		return nil, nil
	}
	hybridIdx, x25519Idx := -1, -1
	for i, share := range ks.KeyShares {
		switch share.Group {
		case utls.X25519MLKEM768:
			hybridIdx = i
		case utls.X25519:
			x25519Idx = i
		}
	}
	if hybridIdx < 0 || x25519Idx < 0 {
		return nil, nil
	}
	x, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate x25519 key share: %w", err)
	}
	dk, err := mlkem.GenerateKey768()
	if err != nil {
		return nil, fmt.Errorf("generate ml-kem-768 key share: %w", err)
	}
	pub := x.PublicKey().Bytes()
	// X25519MLKEM768 client share = ML-KEM-768 encapsulation key || X25519
	// public key (draft-kwiatkowski-tls-ecdhe-mlkem).
	hybrid := make([]byte, 0, mlkem.EncapsulationKeySize768+len(pub))
	hybrid = append(hybrid, dk.EncapsulationKey().Bytes()...)
	hybrid = append(hybrid, pub...)
	shares := make([]utls.KeyShare, len(ks.KeyShares))
	copy(shares, ks.KeyShares)
	shares[hybridIdx].Data = hybrid
	shares[x25519Idx].Data = append([]byte(nil), pub...)
	ks.KeyShares = shares
	return &hybridKeyShare{x25519: x, mlkem: dk}, nil
}
