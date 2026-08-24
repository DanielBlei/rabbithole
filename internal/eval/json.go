// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"io"
)

// WriteJSON writes the machine-readable report.
//
// Every number the other renderers show lives under results, at a stable key,
// so a script reads .results.qwk rather than hunting for a label. The metric
// descriptions are deliberately not here: they are wording for a reader, they
// would need keeping in step with results, and nothing consumes them.
//
// The config block is provenance rather than something to diff — it carries a
// timestamp and an elapsed time that differ on every run by design.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
