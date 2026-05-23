package common

import (
	"strings"
	"io"
	"errors"
	"encoding/base64"
	"crypto/rand"
	"crypto/cipher"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

func GenerateHMACWithKey(key []byte, data string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func GenerateHMAC(data string) string {
	h := hmac.New(sha256.New, []byte(CryptoSecret))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func Password2Hash(password string) (string, error) {
	passwordBytes := []byte(password)
	hashedPassword, err := bcrypt.GenerateFromPassword(passwordBytes, bcrypt.DefaultCost)
	return string(hashedPassword), err
}

func ValidatePasswordAndHash(password string, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// ============================================================
// SECURITY (H-3): Reversible field-level encryption (AES-GCM)
// 用于 channel.Key 等需要可还原的敏感字段。
// 加密结果以 "enc:v1:" 前缀标识，便于判断是否已加密、做平滑迁移。
// 密钥来自 CryptoSecret (env CRYPTO_SECRET)，对其做 SHA-256 派生 32 字节 key。
// ============================================================

const encryptedFieldPrefix = "enc:v1:"

func deriveAESKey() []byte {
	sum := sha256.Sum256([]byte(CryptoSecret))
	return sum[:]
}

// EncryptField 用 AES-GCM 加密一段明文。结果带 "enc:v1:" 前缀，自带 nonce。
// 输入空串返回空串。
func EncryptField(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if strings.HasPrefix(plaintext, encryptedFieldPrefix) {
		// 已经加密过，幂等
		return plaintext, nil
	}
	block, err := aes.NewCipher(deriveAESKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	cipherBytes := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encryptedFieldPrefix + base64.StdEncoding.EncodeToString(cipherBytes), nil
}

// DecryptField 解密 EncryptField 的输出；
// 若输入不带 prefix，视为旧明文数据，原样返回（向后兼容 + 迁移期使用）。
func DecryptField(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, encryptedFieldPrefix) {
		return stored, nil // legacy plaintext — caller should trigger migration
	}
	raw, err := base64.StdEncoding.DecodeString(stored[len(encryptedFieldPrefix):])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(deriveAESKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("encrypted field too short")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// IsFieldEncrypted 判断一段存储值是否已经被加密（含 enc:v1: 前缀）
func IsFieldEncrypted(stored string) bool {
	return strings.HasPrefix(stored, encryptedFieldPrefix)
}
