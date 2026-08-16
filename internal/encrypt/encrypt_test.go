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

	tampered := tamperLastCharacter(ciphertext)
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

func tamperLastCharacter(value string) string {
	replacement := "A"
	if strings.HasSuffix(value, replacement) {
		replacement = "B"
	}
	return value[:len(value)-1] + replacement
}
