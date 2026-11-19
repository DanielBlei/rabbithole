// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package httpclient

import (
	"strings"
	"testing"
)

func TestCredentialIDIsStableAndOpaque(t *testing.T) {
	first := CredentialID("secret-one")
	if first == "" {
		t.Fatal("CredentialID(secret-one) = empty")
	}
	if again := CredentialID("secret-one"); again != first {
		t.Fatalf("CredentialID is unstable: %q then %q", first, again)
	}
	if other := CredentialID("secret-two"); other == first {
		t.Fatal("different credentials share a discriminator")
	}
	if strings.Contains(first, "secret") {
		t.Fatalf("discriminator exposes the credential: %q", first)
	}
	if got := CredentialID(""); got != "" {
		t.Fatalf("CredentialID(\"\") = %q, want empty", got)
	}
}
