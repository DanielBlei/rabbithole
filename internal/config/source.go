// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import "context"

// Source supplies the run config in force. Callers take one snapshot per unit of work.
type Source interface {
	Active(ctx context.Context) (cfg Config, version int, err error)
}

// Static is a Source that always returns the same config at version 0. Each call
// returns a copy, so a caller may adjust it freely.
type Static Config

// Active returns the fixed config.
func (s Static) Active(context.Context) (Config, int, error) {
	return Config(s).Clone(), 0, nil
}
