// Package crypto provides cryptographic utilities for GridKV message signing.
//
// This package provides Ed25519-based message signing and verification for secure
// cluster communication. All messages between nodes are signed to prevent tampering.
//
// Features:
//   - Ed25519 key pair generation
//   - Message signing and verification
//   - Zero external dependencies (uses standard library)
//
// Thread-safety: All functions are safe for concurrent access.
package crypto

import (
	"crypto/ed25519"
	crand "crypto/rand"
)

// Re-export ed25519 types for convenience
type (
	PublicKey  = ed25519.PublicKey
	PrivateKey = ed25519.PrivateKey
)

// KeyPair represents an Ed25519 public-private key pair for message signing and verification.
type KeyPair struct {
	Pub  PublicKey  // Public key for signature verification
	Priv PrivateKey // Private key for message signing
}

// GenerateKeyPair creates a new Ed25519 key pair for secure message signing.
// This is typically called once during node initialization.
//
// Returns:
//   - *KeyPair: The generated key pair
//   - error: Any error encountered during key generation
func GenerateKeyPair() (*KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		return nil, err
	}
	return &KeyPair{Pub: pub, Priv: priv}, nil
}

// SignMessage signs a message using the Ed25519 private key.
// The signature can be verified using VerifyMessage with the corresponding public key.
//
// Parameters:
//   - priv: The private key to sign with
//   - payload: The message to sign
//
// Returns:
//   - []byte: The signature
func SignMessage(priv PrivateKey, payload []byte) []byte {
	return ed25519.Sign(priv, payload)
}

// VerifyMessage verifies an Ed25519 signature.
//
// Parameters:
//   - pub: The public key to verify with
//   - payload: The original message
//   - sig: The signature to verify
//
// Returns:
//   - bool: true if the signature is valid, false otherwise
func VerifyMessage(pub PublicKey, payload, sig []byte) bool {
	return ed25519.Verify(pub, payload, sig)
}
