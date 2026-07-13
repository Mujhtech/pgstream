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

func TestAesCfbRoundTripRejectsShortAndTamperedCiphertext(t *testing.T) {
	cipher, err := NewAesCfb(testEncryptionKey)
	if err != nil {
		t.Fatalf("create cipher: %v", err)
	}
	ciphertext, err := cipher.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	decrypted, err := cipher.Decrypt(ciphertext)
	if err != nil || decrypted != "secret" {
		t.Fatalf("decrypt: plaintext=%q err=%v", decrypted, err)
	}
	if _, err := cipher.Decrypt(aesCFBEnvelopePrefix + "AA"); err == nil {
		t.Fatal("expected short ciphertext to fail")
	}
	tampered := tamperLastCharacter(ciphertext)
	if _, err := cipher.Decrypt(tampered); err == nil {
		t.Fatal("expected tampered CFB ciphertext to fail")
	}
}

func tamperLastCharacter(value string) string {
	replacement := "A"
	if strings.HasSuffix(value, replacement) {
		replacement = "B"
	}
	return value[:len(value)-1] + replacement
}
