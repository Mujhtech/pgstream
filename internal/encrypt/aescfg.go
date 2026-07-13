package encrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

type AesCfb struct {
	block  cipher.Block
	macKey []byte
}

const aesCFBEnvelopePrefix = "pgstream:cfb:v1:"

func NewAesCfb(key string) (Encrypt, error) {
	keyBytes, err := decodeAES256Key(key)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return nil, err
	}
	macKey := sha256.Sum256(append([]byte("pgstream-cfb-authentication:"), keyBytes...))
	return &AesCfb{block: block, macKey: macKey[:]}, nil
}

func (e *AesCfb) Encrypt(plainText []byte) (string, error) {
	const maxSize = 64 * 1024 * 1024 // 64 MB
	if len(plainText) > maxSize {
		return "", fmt.Errorf("plainText too large")
	}

	payload := make([]byte, aes.BlockSize+len(plainText))
	iv := payload[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}

	encrypter := cipher.NewCFBEncrypter(e.block, iv)
	encrypter.XORKeyStream(payload[aes.BlockSize:], plainText)

	mac := hmac.New(sha256.New, e.macKey)
	_, _ = mac.Write(payload)
	payload = append(payload, mac.Sum(nil)...)
	return aesCFBEnvelopePrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func (e *AesCfb) Decrypt(cipherText string) (string, error) {
	if !strings.HasPrefix(cipherText, aesCFBEnvelopePrefix) {
		return "", fmt.Errorf("unsupported or unencrypted ciphertext envelope")
	}

	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(cipherText, aesCFBEnvelopePrefix))
	if err != nil {
		return "", fmt.Errorf("decode ciphertext envelope: %w", err)
	}
	if len(payload) < aes.BlockSize+sha256.Size {
		return "", fmt.Errorf("ciphertext too short")
	}

	message := payload[:len(payload)-sha256.Size]
	receivedMAC := payload[len(payload)-sha256.Size:]
	mac := hmac.New(sha256.New, e.macKey)
	_, _ = mac.Write(message)
	if !hmac.Equal(receivedMAC, mac.Sum(nil)) {
		return "", fmt.Errorf("authenticate ciphertext: invalid message authentication code")
	}

	iv := message[:aes.BlockSize]
	plaintext := append([]byte(nil), message[aes.BlockSize:]...)

	decrypter := cipher.NewCFBDecrypter(e.block, iv)
	decrypter.XORKeyStream(plaintext, plaintext)

	return string(plaintext), nil
}
