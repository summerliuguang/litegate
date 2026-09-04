// Package cryptoutil 提供渠道凭证的 AES-256-GCM 加解密与随机串生成。
package cryptoutil

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const prefix = "enc:v1:"

// Encrypt 加密文本，返回带前缀的十六进制密文。
func Encrypt(plain string, key []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plain), nil)
	return prefix + hex.EncodeToString(append(nonce, ct...)), nil
}

// Decrypt 解密 Encrypt 的输出；不带前缀的值视为历史明文，原样返回。
func Decrypt(stored string, key []byte) (string, error) {
	if !strings.HasPrefix(stored, prefix) {
		return stored, nil
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(stored, prefix))
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// RandomHex 返回 n 字节的十六进制随机串（长度为 2n 的字符串）。
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("system rand unavailable: %v", err))
	}
	return hex.EncodeToString(b)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("cryptoutil: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
