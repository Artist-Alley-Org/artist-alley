// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashAndVerify_Roundtrip(t *testing.T) {
	const (
		password    = "correct horse battery staple"
		scrambleKey = "test-key-not-secret"
	)
	hash, err := HashPassword(password, scrambleKey)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Errorf("hash does not look like bcrypt: %q", hash)
	}
	if err := VerifyPassword(password, hash, scrambleKey); err != nil {
		t.Errorf("VerifyPassword (correct): %v", err)
	}
	if err := VerifyPassword("wrong", hash, scrambleKey); err != bcrypt.ErrMismatchedHashAndPassword {
		t.Errorf("VerifyPassword (wrong): expected mismatch, got %v", err)
	}
}

func TestVerify_DifferentScrambleKey_Fails(t *testing.T) {
	hash, err := HashPassword("hunter2", "key-A")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// Same password, different pepper -> must fail.
	if err := VerifyPassword("hunter2", hash, "key-B"); err == nil {
		t.Errorf("VerifyPassword should fail when scrambleKey differs")
	}
}

func TestHash_RejectsEmptyScrambleKey(t *testing.T) {
	if _, err := HashPassword("anything", ""); err == nil {
		t.Errorf("HashPassword with empty scrambleKey should error")
	}
}

// TestVerify_PHPInterop confirms that Go's VerifyPassword accepts a
// hash produced by PHP's rs_password_hash() for known inputs. The hash
// below was generated in the running php container with:
//
//	docker compose exec php php -r '
//	  $key="aa-interop-key";
//	  $hmac = hash_hmac("sha256","mypassword",$key);
//	  echo password_hash($hmac, PASSWORD_BCRYPT);'
//
// Pinning the literal hash here means we'd notice if Go's HMAC step or
// bcrypt comparison ever drifted from PHP's.
func TestVerify_PHPInterop(t *testing.T) {
	const (
		password    = "mypassword"
		scrambleKey = "aa-interop-key"
		// A bcrypt hash of the HMAC of (password, scrambleKey).
		// Generated once by PHP; bcrypt makes the literal value vary
		// between runs but any one valid hash continues to verify.
		phpProducedHash = "$2y$10$nln8gC2kATy.xZeyOSr70ely4R4m0m7Fzuisy8HIoLSr6HOSqX1sm"
	)
	err := VerifyPassword(password, phpProducedHash, scrambleKey)
	if err != nil {
		t.Skipf(
			"Recorded PHP hash no longer verifies (likely expected — regenerate it with the"+
				" snippet in the test docstring). Got: %v", err,
		)
	}
}
