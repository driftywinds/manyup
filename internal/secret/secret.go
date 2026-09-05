// Package secret provides encryption and hashing for sensitive config values.
// Credentials stored in config.json are encrypted with AES-256-GCM using a
// machine-local master key. The master key is derived from host-specific
// entropy and never leaves the machine.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MasterKeyFile is the path to the file that stores the machine-local master
// key used to encrypt/decrypt credentials in config.json.
const MasterKeyFile = "master.key"

// ConfigDir returns the platform-appropriate config directory. It mirrors
// config.ConfigDir() so the master key lives alongside config.json.
func ConfigDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "manyup")
	}
	if home := os.Getenv("APPDATA"); home != "" {
		return filepath.Join(home, "manyup")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "manyup")
}

// masterKeyPath returns the full path to the master key file.
func masterKeyPath() string {
	return filepath.Join(ConfigDir(), MasterKeyFile)
}

// MasterKey is a 32-byte key used for AES-256-GCM.
type MasterKey [32]byte

// EnsureMasterKey creates the master key file if it does not exist and returns
// the key. It is safe to call multiple times.
func EnsureMasterKey() (MasterKey, error) {
	path := masterKeyPath()
	data, err := os.ReadFile(path)
	if err == nil {
		// Existing key file — decode it.
		var key MasterKey
		if err := json.Unmarshal(data, &key); err != nil {
			return MasterKey{}, fmtErr("reading master key: %w", err)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return MasterKey{}, fmtErr("reading master key: %w", err)
	}

	// Generate a new key.
	var key MasterKey
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		return MasterKey{}, fmtErr("generating master key: %w", err)
	}

	// Ensure the parent directory exists.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return MasterKey{}, fmtErr("creating master key dir: %w", err)
	}

	// Write it to disk with restricted permissions.
	encoded, err := json.Marshal(key)
	if err != nil {
		return MasterKey{}, fmtErr("encoding master key: %w", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return MasterKey{}, fmtErr("writing master key: %w", err)
	}

	return key, nil
}

// EncryptedPrefix is prepended to encrypted values so the config layer can
// distinguish ciphertext from plaintext on Load().
const EncryptedPrefix = "enc://"

// Encrypt takes a plaintext string and returns a value prefixed with
// EncryptedPrefix followed by a base64-encoded ciphertext blob (nonce
// prepended). Decrypt recognizes this prefix and strips it before decrypting.
func Encrypt(key MasterKey, plaintext string) (string, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmtErr("cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmtErr("gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmtErr("nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return EncryptedPrefix + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt takes a value produced by Encrypt (i.e. EncryptedPrefix +
// base64 ciphertext) and returns the plaintext. If the value does not start
// with EncryptedPrefix, it is returned as-is (plaintext passthrough).
func Decrypt(key MasterKey, encoded string) (string, error) {
	if !strings.HasPrefix(encoded, EncryptedPrefix) {
		return encoded, nil
	}
	b64 := encoded[len(EncryptedPrefix):]
	ciphertext, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmtErr("base64 decode: %w", err)
	}

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmtErr("cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmtErr("gcm: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("secret: ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmtErr("decrypt: %w", err)
	}

	return string(plaintext), nil
}

// Hash returns a SHA-256 hash of the given value, hex-encoded. This is useful
// for fingerprinting a credential without storing it in reversible form (e.g.
// for change detection). Unlike Encrypt, Hash is one-way.
func Hash(value string) string {
	h := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", h[:])
}

func fmtErr(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
}
