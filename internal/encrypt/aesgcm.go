package encrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

type AesGcm struct {
	block cipher.Block
}

const aesGCMEnvelopePrefix = "pgstream:gcm:v1:"

func NewAesGcm(key string) (Encrypt, error) {
	keyByte, err := decodeAES256Key(key)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(keyByte)
	if err != nil {
		return nil, err
	}
	return &AesGcm{block: block}, nil
}

func (e *AesGcm) Encrypt(plaintext []byte) (string, error) {
	gcm, err := cipher.NewGCM(e.block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	_, err = io.ReadFull(rand.Reader, nonce)
	if err != nil {
		return "", err
	}

	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return aesGCMEnvelopePrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (e *AesGcm) Decrypt(ciphertext string) (string, error) {
	if !strings.HasPrefix(ciphertext, aesGCMEnvelopePrefix) {
		return "", fmt.Errorf("unsupported or unencrypted ciphertext envelope")
	}

	ciphertextBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ciphertext, aesGCMEnvelopePrefix))
	if err != nil {
		return "", fmt.Errorf("decode ciphertext envelope: %w", err)
	}

	gcm, err := cipher.NewGCM(e.block)
	if err != nil {
		return "", err
	}

	if len(ciphertextBytes) < gcm.NonceSize()+gcm.Overhead() {
		return "", fmt.Errorf("ciphertext too short")
	}

	plaintext, err := gcm.Open(nil,
		ciphertextBytes[:gcm.NonceSize()],
		ciphertextBytes[gcm.NonceSize():],
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("authenticate ciphertext: %w", err)
	}
	return string(plaintext), nil
}

func IsEncryptedCiphertext(value string) bool {
	return strings.HasPrefix(value, "pgstream:")
}
