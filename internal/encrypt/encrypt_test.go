package encrypt

import (
	"encoding/base64"
	"strings"
	"testing"
)

const testEncryptionKey = "0123456789abcdef0123456789abcdef"

func TestAesGcmRoundTripAndAuthentication(t *testing.T) {
	cipher, err := NewAesGcm(testEncryptionKey)
	if err != nil {
		t.Fatalf("create cipher: %v", err)
	}

	plaintext := `{"password":"correct horse battery staple"}`
	ciphertext, err := cipher.Encrypt([]byte(plaintext))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !IsEncryptedCiphertext(ciphertext) || strings.Contains(ciphertext, "correct horse") {
		t.Fatalf("ciphertext envelope is not safe: %q", ciphertext)
	}
	decrypted, err := cipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if decrypted != plaintext {
		t.Fatalf("unexpected plaintext\nwant: %s\n got: %s", plaintext, decrypted)
	}

	tampered := tamperCiphertext(ciphertext)
	if _, err := cipher.Decrypt(tampered); err == nil {
		t.Fatal("expected tampered ciphertext to fail authentication")
	}
	if _, err := cipher.Decrypt(plaintext); err == nil {
		t.Fatal("expected plaintext to be rejected by decrypt")
	}
}

func TestAesGcmAcceptsBase64EncodedKey(t *testing.T) {
	encodedKey := base64.StdEncoding.EncodeToString([]byte(testEncryptionKey))
	if _, err := NewAesGcm(encodedKey); err != nil {
		t.Fatalf("create cipher from base64 key: %v", err)
	}
}

// tamperCiphertext flips a character in the middle of the base64 payload.
// The final character carries only four data bits plus slack, so changing it
// can decode to identical bytes; a middle character always alters the bytes.
func tamperCiphertext(value string) string {
	position := len(value) - 8
	replacement := byte('A')
	if value[position] == replacement {
		replacement = 'B'
	}
	return value[:position] + string(replacement) + value[position+1:]
}
