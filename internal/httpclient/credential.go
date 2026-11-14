// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package httpclient

import (
	"crypto/sha256"
	"encoding/hex"
)

// CredentialID returns a stable, non-reversible discriminator for one API
// credential. It exists so in-process capability caches can distinguish
// credentials without ever storing, logging or persisting the credential
// itself. It is not an authentication primitive.
//
// The empty credential maps to the empty string, so credential-less endpoints
// share one deterministic cache key. Callers must use the credential actually
// resolved for the request, not the name of the environment variable that
// supplied it.
func CredentialID(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}
