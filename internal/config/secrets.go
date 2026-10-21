// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"regexp"
	"strings"
)

// Canonical environment names for the api_key fields.
const inferenceKeyField = "inference.api_key"

// allowedNameRE matches every variable the .env loader reads (RABBITHOLE_DB and
// RABBITHOLE_DB_PASSWORD included). keyRefRE is the narrower positive list for
// what a stored config may reference and the .env writer may touch: api keys
// only, so a config can never point a key at the store URL or its password.
var (
	allowedNameRE = regexp.MustCompile(`^RABBITHOLE_[A-Z0-9_]+$`)
	keyRefRE      = regexp.MustCompile(`^RABBITHOLE_[A-Z0-9_]*_API_KEY$`)
)

// RefFor returns the canonical environment variable name for an api_key field,
// or "" for any other field.
func RefFor(field string) string {
	if field == inferenceKeyField {
		return "RABBITHOLE_INFERENCE_API_KEY"
	}
	return ""
}

// allowedName reports whether name may be referenced from a stored config or
// written to the .env file.
func allowedName(name string) bool { return keyRefRE.MatchString(name) }

// RefName returns the variable name of a whole $NAME or ${NAME} reference.
func RefName(value string) (name string, ok bool) { return parseRef(value) }

// ValidRef accepts only a whole $NAME or ${NAME} where NAME is RABBITHOLE_..._API_KEY. The error never contains
// the value.
func ValidRef(value string) error {
	hint := errors.New("an api_key must be a reference to a RABBITHOLE_*_API_KEY environment variable, " +
		"for example: export RABBITHOLE_INFERENCE_API_KEY=$YOUR_VAR and set api_key: $RABBITHOLE_INFERENCE_API_KEY")
	name, ok := parseRef(value)
	if !ok || !allowedName(name) {
		return hint
	}
	return nil
}

// Secret is a literal api_key lifted out of a config, to be written to .env
// under Name. Value must never be logged.
type Secret struct {
	Field string
	Name  string
	Value string
}

// ExtractLiterals replaces each non-empty api_key that is not a $ reference with
// a reference to its canonical variable, and returns the literals so the caller
// can store them in .env. Run it before Prepare, which rejects literals.
func (r *Config) ExtractLiterals() []Secret {
	key := &r.Inference.APIKey
	if *key == "" || strings.HasPrefix(*key, "$") {
		return nil
	}
	name := RefFor(inferenceKeyField)
	sec := Secret{Field: inferenceKeyField, Name: name, Value: *key}
	*key = "$" + name
	return []Secret{sec}
}

// ResolvedAPIKey resolves the inference api_key reference through the
// environment at use time. An empty key resolves to "".
func (c InferenceConfig) ResolvedAPIKey() (string, error) {
	return expandEnvRef(inferenceKeyField, c.APIKey)
}
