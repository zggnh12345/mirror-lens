// Package secret 负责设置页里敏感字段的落盘加密。
//
// 目标不是防御拿到整块磁盘的人，而是让 token 不以明文形式出现在
// config.yaml、配置备份、git 历史和日志里。密钥单独存放在
// <data>/secret.key（0600），密文放配置文件的 *_enc 字段。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeySize 是 AES-256 的密钥长度。
const KeySize = 32

// aad 把密文绑定到用途，避免同一个密钥下不同字段的密文互换。
const aad = "mirror-lens/secret/v1"

// LoadOrCreateKey 读取密钥文件；不存在就生成一个 32 字节随机密钥。
// 文件权限固定 0600，目录 0700；已存在的文件不覆盖、不改权限。
func LoadOrCreateKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("密钥路径为空")
	}
	b, err := os.ReadFile(path)
	if err == nil {
		key, derr := decodeKey(b)
		if derr != nil {
			return nil, fmt.Errorf("密钥文件 %s 损坏: %w", path, derr)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读密钥文件: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建密钥目录: %w", err)
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成密钥: %w", err)
	}
	// O_EXCL：并发首次保存时只有一个能写成，另一个读回同一把钥匙。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil, fmt.Errorf("读密钥文件: %w", rerr)
			}
			return decodeKey(b)
		}
		return nil, fmt.Errorf("创建密钥文件: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		return nil, fmt.Errorf("写密钥文件: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("同步密钥文件: %w", err)
	}
	// 创建时的 0600 在部分文件系统上不生效（实测落到 755），显式再收一次。
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("收紧密钥文件权限: %w", err)
	}
	return key, nil
}

func decodeKey(b []byte) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, err
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("长度应为 %d 字节，实际 %d", KeySize, len(key))
	}
	return key, nil
}

// Encrypt 返回 base64(nonce || ciphertext)。
func Encrypt(key []byte, plain string) (string, error) {
	gcm, err := aead(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成随机数: %w", err)
	}
	out := gcm.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt 解开 Encrypt 的输出；密文被改动、密钥不匹配都返回错误。
func Decrypt(key []byte, enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	gcm, err := aead(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("密文长度不足")
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, []byte(aad))
	if err != nil {
		return "", errors.New("解密失败（密钥不匹配或密文被改动）")
	}
	return string(plain), nil
}

func aead(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("密钥长度应为 %d 字节", KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
