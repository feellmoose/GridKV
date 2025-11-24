package crypto

import (
	"testing"
)

func TestGenerateKeyPair(t *testing.T) {
	keypair, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("Failed to generate keypair: %v", err)
	}

	if keypair.Pub == nil {
		t.Error("Expected public key, got nil")
	}
	if keypair.Priv == nil {
		t.Error("Expected private key, got nil")
	}

	if len(keypair.Pub) != 32 {
		t.Errorf("Expected 32-byte public key, got %d bytes", len(keypair.Pub))
	}
	if len(keypair.Priv) != 64 {
		t.Errorf("Expected 64-byte private key, got %d bytes", len(keypair.Priv))
	}
}

func TestSignVerify(t *testing.T) {
	keypair, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("Failed to generate keypair: %v", err)
	}

	message := []byte("test message")
	signature := SignMessage(keypair.Priv, message)

	if len(signature) != 64 {
		t.Errorf("Expected 64-byte signature, got %d bytes", len(signature))
	}

	valid := VerifyMessage(keypair.Pub, message, signature)
	if !valid {
		t.Error("Signature verification failed")
	}

	invalidMessage := []byte("different message")
	valid = VerifyMessage(keypair.Pub, invalidMessage, signature)
	if valid {
		t.Error("Signature verification should fail for different message")
	}
}

func TestSignVerify_MultipleKeyPairs(t *testing.T) {
	keypair1, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("Failed to generate keypair1: %v", err)
	}

	keypair2, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("Failed to generate keypair2: %v", err)
	}

	if len(keypair1.Pub) == 0 || len(keypair2.Pub) == 0 {
		t.Fatal("Keypairs should have public keys")
	}

	equal := true
	for i := range keypair1.Pub {
		if keypair1.Pub[i] != keypair2.Pub[i] {
			equal = false
			break
		}
	}
	if equal {
		t.Error("Different keypairs should have different public keys")
	}

	message := []byte("test message")
	sig1 := SignMessage(keypair1.Priv, message)
	sig2 := SignMessage(keypair2.Priv, message)

	equal = true
	for i := range sig1 {
		if sig1[i] != sig2[i] {
			equal = false
			break
		}
	}
	if equal {
		t.Error("Different keys should produce different signatures")
	}

	valid := VerifyMessage(keypair1.Pub, message, sig1)
	if !valid {
		t.Error("Signature 1 should verify with keypair1")
	}

	valid = VerifyMessage(keypair2.Pub, message, sig1)
	if valid {
		t.Error("Signature 1 should not verify with keypair2")
	}
}

func TestSignVerify_EmptyMessage(t *testing.T) {
	keypair, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("Failed to generate keypair: %v", err)
	}

	message := []byte{}
	signature := SignMessage(keypair.Priv, message)

	if len(signature) != 64 {
		t.Errorf("Expected 64-byte signature, got %d bytes", len(signature))
	}

	valid := VerifyMessage(keypair.Pub, message, signature)
	if !valid {
		t.Error("Signature verification failed for empty message")
	}
}

func TestSignVerify_LargeMessage(t *testing.T) {
	keypair, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("Failed to generate keypair: %v", err)
	}

	message := make([]byte, 1024*1024)
	for i := range message {
		message[i] = byte(i % 256)
	}

	signature := SignMessage(keypair.Priv, message)

	if len(signature) != 64 {
		t.Errorf("Expected 64-byte signature, got %d bytes", len(signature))
	}

	valid := VerifyMessage(keypair.Pub, message, signature)
	if !valid {
		t.Error("Signature verification failed for large message")
	}

	message[0] = ^message[0]
	valid = VerifyMessage(keypair.Pub, message, signature)
	if valid {
		t.Error("Signature verification should fail for modified message")
	}
}

func TestSignVerify_Concurrent(t *testing.T) {
	keypair, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("Failed to generate keypair: %v", err)
	}

	const numGoroutines = 50
	const opsPerGoroutine = 100

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			for j := 0; j < opsPerGoroutine; j++ {
				message := []byte{byte(id), byte(j)}
				signature := SignMessage(keypair.Priv, message)
				valid := VerifyMessage(keypair.Pub, message, signature)
				if !valid {
					t.Errorf("Signature verification failed for goroutine %d, op %d", id, j)
				}
			}
		}(i)
	}
}
