package auth

import (
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	encoded, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Fatalf("unexpected encoding: %s", encoded)
	}

	ok, err := Verify("correct horse battery staple", encoded)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("the correct secret did not verify")
	}

	ok, err = Verify("wrong", encoded)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Fatal("a wrong secret verified")
	}
}

// Two hashes of one secret must differ: an equal pair would mean the salt is
// not being applied.
func TestHashIsSalted(t *testing.T) {
	a, err := Hash("same")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	b, err := Hash("same")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of one secret are identical; the salt is not applied")
	}
}

// Parameters are read from the stored hash, not from this package's constants,
// so raising the cost later must leave existing credentials verifiable.
func TestVerifyHonoursStoredParameters(t *testing.T) {
	encoded, err := Hash("secret")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	// Rewrite the cost fields to values this build would never choose; the
	// digest no longer matches, but parsing must still succeed rather than
	// reporting a malformed hash.
	tampered := strings.Replace(encoded, "m=65536,t=1,", "m=32768,t=2,", 1)
	if tampered == encoded {
		t.Skip("hash format changed; update this test with it")
	}
	if _, err := Verify("secret", tampered); err != nil {
		t.Fatalf("verify should parse foreign parameters, got: %v", err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"not enough parts": "$argon2id$v=19$m=1,t=1,p=1$salt",
		"wrong algorithm":  "$bcrypt$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA",
		"bad version":      "$argon2id$v=13$m=1,t=1,p=1$c2FsdA$aGFzaA",
		"bad params":       "$argon2id$v=19$nonsense$c2FsdA$aGFzaA",
		"bad salt base64":  "$argon2id$v=19$m=1,t=1,p=1$!!!$aGFzaA",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := Verify("secret", encoded)
			if err == nil {
				t.Fatalf("expected an error, got ok=%t", ok)
			}
			if ok {
				t.Fatal("a malformed hash must never verify")
			}
		})
	}
}

func TestNewNodeTokenIsPrefixedAndUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for range 32 {
		token, err := NewNodeToken()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !strings.HasPrefix(token, TokenPrefix) {
			t.Fatalf("token %q lacks the %q prefix", token, TokenPrefix)
		}
		if len(token) < len(TokenPrefix)+40 {
			t.Fatalf("token %q is shorter than 32 bytes of entropy implies", token)
		}
		if _, dup := seen[token]; dup {
			t.Fatalf("minted a duplicate token: %s", token)
		}
		seen[token] = struct{}{}
	}
}
