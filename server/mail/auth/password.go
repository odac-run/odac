// Package auth implements authentication and security primitives for the mail server.
// Password hashing uses scrypt with timing-safe comparison, maintaining full
// backward compatibility with the existing Node.js scrypt$salt$key format.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const (
	saltLen    = 16
	keyLen     = 64
	scryptN    = 32768 // CPU/memory cost parameter
	scryptR    = 8     // Block size
	scryptP    = 1     // Parallelization
)

// HashPassword generates a scrypt hash in the format: scrypt$<saltHex>$<keyHex>
// This format is identical to the Node.js Mail.js implementation for backward compatibility.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt generation failed: %w", err)
	}

	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, keyLen)
	if err != nil {
		return "", fmt.Errorf("scrypt derivation failed: %w", err)
	}

	return fmt.Sprintf("scrypt$%s$%s", hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

// ComparePassword verifies a plaintext password against a stored scrypt hash.
// Uses timing-safe comparison to prevent timing attacks.
// Returns false gracefully for unrecognized hash formats (e.g., legacy bcrypt).
func ComparePassword(password, storedHash string) (bool, error) {
	if !strings.HasPrefix(storedHash, "scrypt$") {
		// Legacy hash format (bcrypt etc.) — cannot verify without the original library
		return false, nil
	}

	parts := strings.SplitN(storedHash, "$", 3)
	if len(parts) != 3 {
		return false, errors.New("malformed scrypt hash")
	}

	salt, err := hex.DecodeString(parts[1])
	if err != nil {
		return false, fmt.Errorf("invalid salt hex: %w", err)
	}

	storedKey, err := hex.DecodeString(parts[2])
	if err != nil {
		return false, fmt.Errorf("invalid key hex: %w", err)
	}

	derivedKey, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, keyLen)
	if err != nil {
		return false, fmt.Errorf("scrypt derivation failed: %w", err)
	}

	if len(derivedKey) != len(storedKey) {
		return false, nil
	}

	return subtle.ConstantTimeCompare(derivedKey, storedKey) == 1, nil
}
