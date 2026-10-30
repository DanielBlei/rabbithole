// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// PlanStatus is what importing one file entry would do.
type PlanStatus string

const (
	PlanNew        PlanStatus = "new"
	PlanSubscribed PlanStatus = "subscribed"
	PlanRemoved    PlanStatus = "removed"
	PlanConflict   PlanStatus = "conflict"
	PlanInvalid    PlanStatus = "invalid"
)

const (
	planReasonRemoved   = "You removed this source. Tick to restore it."
	planReasonNameTaken = "Name taken by a source with a different URL. Add it under another name from the Add form."
	planReasonNameGone  = "A removed source uses this name."
	planReasonIDTaken   = "One of your sources used this address before you changed it."
	planReasonDuplicate = "Appears more than once in this file."
)

// ErrNothingToImport means an apply picked no row that could be added or restored.
var ErrNothingToImport = errors.New("nothing selected")

// PlanRow is one file entry with its verdict. Feed carries the normalized
// values an apply would store; Key is its config.FeedURLKey, which is how a
// pick names the row. ExistingName is the stored name of a subscribed match.
type PlanRow struct {
	Feed         config.Feed
	Key          string
	ExistingName string
	Status       PlanStatus
	Reason       string

	// ID is the stored feed a subscribed or removed entry matches; empty otherwise.
	ID string

	// IDTaken is set when a source whose address was edited away from this one
	// still holds its id, so the entry cannot be added under any name. It wins over
	// a name conflict, which a rename would not clear.
	IDTaken bool
}

// ApplyResult counts what an apply did. Skipped is picks that no longer plan as
// new or removed, or that name no row of the file.
type ApplyResult struct {
	Added, Restored, Skipped int
}

// FeedMatchState says whether an address is already a stored feed.
type FeedMatchState string

const (
	FeedMatchNone    FeedMatchState = "none"
	FeedMatchLive    FeedMatchState = "live"
	FeedMatchRemoved FeedMatchState = "removed"
)

// FeedMatch is the stored feed an address stands for. ID and Name are empty
// when State is FeedMatchNone.
type FeedMatch struct {
	State FeedMatchState
	ID    string
	Name  string
}

// LookupFeedURL finds the stored feed rawURL points at, by config.FeedURLKey,
// so a trailing slash or host case does not hide it. A live feed wins over a
// removed twin; among removed twins the lowest id does.
func (s *Store) LookupFeedURL(ctx context.Context, rawURL string) (FeedMatch, error) {
	key := config.FeedURLKey(config.NormalizeFeedURL(rawURL))
	match := FeedMatch{State: FeedMatchNone}
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := s.txQuery(ctx, tx, sqlFeedsForImport)
		if err != nil {
			return fmt.Errorf("query feeds for lookup: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, name, url string
			var live bool
			if err := rows.Scan(&id, &name, &url, &live); err != nil {
				return fmt.Errorf("scan feeds for lookup: %w", err)
			}
			if config.FeedURLKey(url) != key {
				continue
			}
			if live {
				match = FeedMatch{State: FeedMatchLive, ID: id, Name: name}
				return nil
			}
			if match.State == FeedMatchNone {
				match = FeedMatch{State: FeedMatchRemoved, ID: id, Name: name}
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("query feeds for lookup: %w", err)
		}
		return nil
	})
	if err != nil {
		return FeedMatch{}, err
	}
	return match, nil
}

// PlanImport classifies every entry of doc against the stored feeds without
// writing anything. Rows come back in file order.
func (s *Store) PlanImport(ctx context.Context, doc config.FeedsDoc) ([]PlanRow, error) {
	var plan []PlanRow
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		plan, err = s.planImportTx(ctx, tx, doc)
		return err
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// planImportTx is the one classifier, shared by the preview and the apply so a
// pick is judged against the same rules at both moments.
func (s *Store) planImportTx(ctx context.Context, tx *sql.Tx, doc config.FeedsDoc) ([]PlanRow, error) {
	type stored struct {
		id, name, url string
		live          bool
	}
	rows, err := s.txQuery(ctx, tx, sqlFeedsForImport)
	if err != nil {
		return nil, fmt.Errorf("query feeds for import: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Rows arrive in id order. A URL key can be shared by several rows only
	// when deleted twins pile up; a live row always wins it.
	byKey := make(map[string][]stored)
	byName := make(map[string]stored)
	ids := make(map[string]bool)
	for rows.Next() {
		var r stored
		if err := rows.Scan(&r.id, &r.name, &r.url, &r.live); err != nil {
			return nil, fmt.Errorf("scan feeds for import: %w", err)
		}
		key := config.FeedURLKey(r.url)
		byKey[key] = append(byKey[key], r)
		byName[r.name] = r
		ids[r.id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query feeds for import: %w", err)
	}

	// matchFor picks the stored row a file entry stands for: the live one,
	// else the deleted twin that already has the file's name, else the first.
	matchFor := func(key, name string) (stored, bool) {
		var best stored
		found := false
		for _, r := range byKey[key] {
			switch {
			case r.live:
				return r, true
			case !found, r.name == name && best.name != name:
				best, found = r, true
			}
		}
		return best, found
	}

	plan := make([]PlanRow, 0, len(doc.Feeds))
	seen := make(map[string]bool)
	for _, raw := range doc.Feeds {
		f, err := validateFeedInput(raw)
		if err != nil {
			f.URL = config.NormalizeFeedURL(raw.URL)
			plan = append(
				plan,
				PlanRow{Feed: f, Key: config.FeedURLKey(f.URL), Status: PlanInvalid, Reason: err.Error()},
			)
			continue
		}
		row := PlanRow{Feed: f, Key: config.FeedURLKey(f.URL)}
		match, hasMatch := matchFor(row.Key, f.Name)
		// A source whose address was edited keeps the id of the old one. A match
		// is updated in place, so only an insert can hit a taken id.
		row.IDTaken = !hasMatch && ids[config.FeedID(f.URL)]
		holder, nameHeld := byName[f.Name]
		nameReason := planReasonNameTaken
		if nameHeld && !holder.live {
			nameReason = planReasonNameGone
		}
		switch {
		case hasMatch && match.live:
			row.Status = PlanSubscribed
			row.ID = match.id
			row.ExistingName = match.name
			row.Reason = "Already in your sources as " + match.name
		case hasMatch:
			row.Status, row.Reason, row.ID = PlanRemoved, planReasonRemoved, match.id
			if nameHeld && holder.id != match.id {
				row.Status, row.Reason, row.ID = PlanConflict, nameReason, ""
			}
		case row.IDTaken:
			row.Status, row.Reason = PlanConflict, planReasonIDTaken
		case nameHeld:
			row.Status, row.Reason = PlanConflict, nameReason
		default:
			row.Status = PlanNew
		}

		if row.Status == PlanNew || row.Status == PlanRemoved {
			nameKey := feedNameKey(f.Name)
			if seen[feedURLKey(f.URL)] || seen[nameKey] {
				row.Status, row.Reason, row.ID = PlanConflict, planReasonDuplicate, ""
			} else {
				seen[feedURLKey(f.URL)] = true
				seen[nameKey] = true
			}
		}
		plan = append(plan, row)
	}
	return plan, nil
}

// ApplyImport adds or restores the entries of doc whose keys are picked, in
// one transaction. The file is planned again inside it, so a pick whose row
// changed since the preview is skipped rather than applied blind. Any failure
// rolls the whole import back.
func (s *Store) ApplyImport(ctx context.Context, doc config.FeedsDoc, pick map[string]bool) (ApplyResult, error) {
	var result ApplyResult
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		plan, err := s.planImportTx(ctx, tx, doc)
		if err != nil {
			return err
		}
		applied := make(map[string]bool)
		for _, row := range plan {
			if !pick[row.Key] || applied[row.Key] {
				continue
			}
			switch row.Status {
			case PlanNew:
				if _, err := s.insertFeedTx(ctx, tx, row.Feed); err != nil {
					return err
				}
				result.Added++
			case PlanRemoved:
				if err := s.restoreFeedTx(ctx, tx, row.ID, row.Feed); err != nil {
					return err
				}
				result.Restored++
			default:
				continue
			}
			applied[row.Key] = true
		}
		for key, on := range pick {
			if on && !applied[key] {
				result.Skipped++
			}
		}
		if result.Added+result.Restored == 0 {
			return ErrNothingToImport
		}
		return nil
	})
	if errors.Is(err, ErrNothingToImport) {
		return result, err
	}
	if err != nil {
		return ApplyResult{}, err
	}
	return result, nil
}
