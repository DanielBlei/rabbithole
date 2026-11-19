// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strings"
)

// identifierChunkSize keeps both IN lists under SQLite's bound-parameter limit.
// Each identifier is supplied once for the id list and once for the link list.
const identifierChunkSize = 400

// ItemIDsByIdentifiers resolves item identifiers to canonical item IDs. An
// identifier may be an item ID or its link, matching Store.Get. Unknown or
// deleted identifiers are absent from the result. The lookup is batched and
// deduplicated so a feed page does not issue one query per row.
func (s *Store) ItemIDsByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error) {
	out := make(map[string]string, len(identifiers))
	if len(identifiers) == 0 {
		return out, nil
	}
	unique := make([]string, 0, len(identifiers))
	seen := make(map[string]struct{}, len(identifiers))
	for _, id := range identifiers {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	type pair struct {
		id   string
		link string
	}
	var pairs []pair
	for start := 0; start < len(unique); start += identifierChunkSize {
		end := min(start+identifierChunkSize, len(unique))
		chunk := unique[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, 2*len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.query(ctx,
			`SELECT id, link FROM items WHERE id IN (`+placeholders+`) OR link IN (`+placeholders+`)`,
			args...)
		if err != nil {
			return nil, fmt.Errorf("resolve item identifiers: %w", err)
		}
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.id, &p.link); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan item identifier: %w", err)
			}
			pairs = append(pairs, p)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate item identifiers: %w", err)
		}
		_ = rows.Close()
	}

	// Match links before IDs, exactly as Store.Get does. A link value that
	// happens to equal another item's ID therefore resolves to the link owner.
	byID := make(map[string]string, len(pairs))
	byLink := make(map[string]string, len(pairs))
	for _, p := range pairs {
		byID[p.id] = p.id
		if p.link != "" {
			byLink[p.link] = p.id
		}
	}
	for _, input := range unique {
		if id, ok := byLink[input]; ok {
			out[input] = id
			continue
		}
		if id, ok := byID[input]; ok {
			out[input] = id
		}
	}
	return out, nil
}
