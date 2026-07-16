// Package auth implements authentication and security primitives for the mail server.
// Password hashing uses adaptive scrypt with automatic format detection:
//   - New format: scrypt$<N>$<saltHex>$<keyHex> (embeds cost parameter for future-proofing)
//   - Legacy format: scrypt$<saltHex>$<keyHex> (Node.js compat, assumes N=16384)
//
// Passwords hashed with the legacy format are automatically upgraded on next
// successful login via the NeedsRehash() check.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const (
	keyLen        = 64
	saltLen       = 16
	scryptN       = 32768 // Current cost parameter (OWASP 2023 recommendation)
	scryptNLegacy = 16384 // Node.js crypto.scrypt default — used for legacy hash verification
	scryptP       = 1     // Parallelization
	scryptR       = 8     // Block size
)

// HashPassword generates a scrypt hash in the adaptive format: scrypt$<N>$<saltHex>$<keyHex>.
// Embeds the N parameter so future cost upgrades are transparent to ComparePassword.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt generation failed: %w", err)
	}

	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, keyLen)
	if err != nil {
		return "", fmt.Errorf("scrypt derivation failed: %w", err)
	}

	return fmt.Sprintf("scrypt$%d$%s$%s", scryptN, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

// ComparePassword verifies a plaintext password against a stored scrypt hash.
// Supports both adaptive (4-part) and legacy (3-part) formats.
// Uses timing-safe comparison to prevent timing attacks.
// Returns false gracefully for unrecognized hash formats (e.g., legacy bcrypt).
func ComparePassword(password, storedHash string) (bool, error) {
	if !strings.HasPrefix(storedHash, "scrypt$") {
		// Legacy hash format (bcrypt etc.) — cannot verify without the original library
		return false, nil
	}

	n, salt, storedKey, err := parseHash(storedHash)
	if err != nil {
		return false, err
	}

	derivedKey, err := scrypt.Key([]byte(password), salt, n, scryptR, scryptP, keyLen)
	if err != nil {
		return false, fmt.Errorf("scrypt derivation failed: %w", err)
	}

	if len(derivedKey) != len(storedKey) {
		return false, nil
	}

	return subtle.ConstantTimeCompare(derivedKey, storedKey) == 1, nil
}

// NeedsRehash returns true if the stored hash should be upgraded to the current
// scryptN cost parameter. This enables transparent password strengthening on login.
func NeedsRehash(storedHash string) bool {
	if !strings.HasPrefix(storedHash, "scrypt$") {
		return true
	}

	parts := strings.SplitN(storedHash, "$", 4)

	// Legacy 3-part format: scrypt$salt$key → always needs rehash
	if len(parts) == 3 {
		return true
	}

	// Adaptive 4-part format: scrypt$N$salt$key → check if N is outdated
	if len(parts) == 4 {
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return true
		}
		return n < scryptN
	}

	return true
}

// parseHash extracts N, salt, and storedKey from both legacy and adaptive formats.
func parseHash(storedHash string) (int, []byte, []byte, error) {
	parts := strings.SplitN(storedHash, "$", 4)

	switch len(parts) {
	case 3:
		// Legacy format: scrypt$<saltHex>$<keyHex> → N=16384 (Node.js default)
		salt, err := hex.DecodeString(parts[1])
		if err != nil {
			return 0, nil, nil, fmt.Errorf("invalid salt hex: %w", err)
		}
		key, err := hex.DecodeString(parts[2])
		if err != nil {
			return 0, nil, nil, fmt.Errorf("invalid key hex: %w", err)
		}
		return scryptNLegacy, salt, key, nil

	case 4:
		// Adaptive format: scrypt$<N>$<saltHex>$<keyHex>
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, nil, nil, fmt.Errorf("invalid N parameter: %w", err)
		}
		salt, err := hex.DecodeString(parts[2])
		if err != nil {
			return 0, nil, nil, fmt.Errorf("invalid salt hex: %w", err)
		}
		key, err := hex.DecodeString(parts[3])
		if err != nil {
			return 0, nil, nil, fmt.Errorf("invalid key hex: %w", err)
		}
		return n, salt, key, nil

	default:
		return 0, nil, nil, errors.New("malformed scrypt hash")
	}
}
