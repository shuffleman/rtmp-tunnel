// crypto/cipher.go - 代理数据加解密
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync/atomic"
)

// Cipher 加密接口
type Cipher interface {
	// Encrypt 加密数据，返回加密后的数据
	Encrypt(plaintext []byte) ([]byte, error)
	// Decrypt 解密数据，返回解密后的明文
	Decrypt(ciphertext []byte) ([]byte, error)
}

// XORCipher 简单XOR加密(仅用于测试或低安全场景)
type XORCipher struct {
	key []byte
}

// NewXORCipher 创建XOR加密器
func NewXORCipher(key []byte) *XORCipher {
	h := sha256.New()
	h.Write(key)
	return &XORCipher{key: h.Sum(nil)}
}

// Encrypt XOR加密
func (c *XORCipher) Encrypt(plaintext []byte) ([]byte, error) {
	ciphertext := make([]byte, len(plaintext))
	for i, b := range plaintext {
		ciphertext[i] = b ^ c.key[i%len(c.key)]
	}
	return ciphertext, nil
}

// Decrypt XOR解密(与加密相同)
func (c *XORCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	return c.Encrypt(ciphertext)
}

// AESCipher AES-GCM加密
type AESCipher struct {
	aead    cipher.AEAD
	prefix  [4]byte
	counter atomic.Uint64
}

// NewAESCipher 创建AES加密器
func NewAESCipher(key []byte) (*AESCipher, error) {
	// 派生32字节密钥
	h := sha256.New()
	h.Write(key)
	derivedKey := h.Sum(nil)

	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		return nil, fmt.Errorf("create aes cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	c := &AESCipher{aead: aead}
	_, _ = rand.Read(c.prefix[:])
	return c, nil
}

// Encrypt AES-GCM加密
func (c *AESCipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	var nonceBuf [32]byte
	if nonceSize > len(nonceBuf) {
		return nil, fmt.Errorf("unsupported nonce size: %d", nonceSize)
	}
	nonce := nonceBuf[:nonceSize]
	copy(nonce, c.prefix[:])
	ctr := c.counter.Add(1)
	if nonceSize >= 12 {
		binary.BigEndian.PutUint64(nonce[nonceSize-8:], ctr)
	} else if nonceSize >= 8 {
		binary.BigEndian.PutUint64(nonce[:8], ctr)
	} else {
		for i := 0; i < nonceSize; i++ {
			nonce[nonceSize-1-i] = byte(ctr >> (8 * i))
		}
	}
	out := make([]byte, nonceSize, nonceSize+len(plaintext)+c.aead.Overhead())
	copy(out, nonce)
	out = c.aead.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// Decrypt AES-GCM解密
func (c *AESCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}

// NewCipher 根据配置创建加密器
func NewCipher(algorithm string, key []byte) (Cipher, error) {
	switch algorithm {
	case "aes-256-gcm", "aes-128-gcm", "aes-gcm":
		return NewAESCipher(key)
	case "xor":
		return NewXORCipher(key), nil
	default:
		// 默认使用AES-GCM
		return NewAESCipher(key)
	}
}
