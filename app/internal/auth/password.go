package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword produces a hash compatible with RS's rs_password_hash():
// HMAC-SHA256(password, scrambleKey) — encoded as lowercase hex —
// then bcrypt that.
//
// PHP equivalent (include/login_functions.php):
//
//	$hmac = hash_hmac('sha256', $password, $GLOBALS['scramble_key']);
//	return password_hash($hmac, PASSWORD_BCRYPT, $options);
func HashPassword(password, scrambleKey string) (string, error) {
	if scrambleKey == "" {
		return "", errors.New("auth: scrambleKey is empty; refusing to hash")
	}
	hmacHex := pepper(password, scrambleKey)
	hashed, err := bcrypt.GenerateFromPassword([]byte(hmacHex), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: bcrypt: %w", err)
	}
	return string(hashed), nil
}

// VerifyPassword compares a candidate password against a stored hash
// using RS's HMAC-then-bcrypt scheme. Returns nil on a successful
// match; bcrypt.ErrMismatchedHashAndPassword on a mismatch; any other
// error indicates a corrupted hash or bad input.
func VerifyPassword(candidate, storedHash, scrambleKey string) error {
	if scrambleKey == "" {
		return errors.New("auth: scrambleKey is empty; refusing to verify")
	}
	hmacHex := pepper(candidate, scrambleKey)
	return bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(hmacHex))
}

// pepper applies the HMAC-SHA256 step exactly as PHP's hash_hmac does
// by default: lower-case hex of the 32-byte digest.
func pepper(password, scrambleKey string) string {
	mac := hmac.New(sha256.New, []byte(scrambleKey))
	mac.Write([]byte(password))
	return hex.EncodeToString(mac.Sum(nil))
}
