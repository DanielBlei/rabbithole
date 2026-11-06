// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MaxStoryItems bounds the item set a clustering run reads.
const MaxStoryItems = 500

// StoryItem is one stored item considered for story clustering.
type StoryItem struct {
	ID        string
	Source    string
	Title     string
	Link      string
	Summary   string
	Published time.Time
	Score     int
}

// StoryCluster is one persisted story group.
type StoryCluster struct {
	ID                   string
	RepresentativeItemID string
	Method               string
	Confidence           float64
	Items                []StoryClusterItem
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// StoryClusterItem is one member of a persisted story cluster.
type StoryClusterItem struct {
	ItemID   string
	Position int
}

const storyClusterSchema = `
CREATE TABLE IF NOT EXISTS story_clusters (
	id                     TEXT PRIMARY KEY,
	representative_item_id TEXT NOT NULL,
	method                 TEXT NOT NULL DEFAULT '',
	confidence             REAL NOT NULL DEFAULT 0,
	created_at             TIMESTAMP NOT NULL,
	updated_at             TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_story_clusters_representative ON story_clusters(representative_item_id);
`

const storyClusterSchemaPG = `
CREATE TABLE IF NOT EXISTS story_clusters (
	id                     TEXT PRIMARY KEY,
	representative_item_id TEXT NOT NULL,
	method                 TEXT NOT NULL DEFAULT '',
	confidence             DOUBLE PRECISION NOT NULL DEFAULT 0,
	created_at             TIMESTAMPTZ NOT NULL,
	updated_at             TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_story_clusters_representative ON story_clusters(representative_item_id);
`

const storyClusterItemSchema = `
CREATE TABLE IF NOT EXISTS story_cluster_items (
	cluster_id TEXT NOT NULL,
	item_id    TEXT NOT NULL,
	item_rank  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (cluster_id, item_id)
);
CREATE INDEX IF NOT EXISTS idx_story_cluster_items_item ON story_cluster_items(item_id);
`

const storyClusterItemSchemaPG = `
CREATE TABLE IF NOT EXISTS story_cluster_items (
	cluster_id TEXT NOT NULL,
	item_id    TEXT NOT NULL,
	item_rank  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (cluster_id, item_id)
);
CREATE INDEX IF NOT EXISTS idx_story_cluster_items_item ON story_cluster_items(item_id);
`

// StoryItems returns stored items for clustering, newest first and bounded.
func (s *Store) StoryItems(ctx context.Context, limit int) ([]StoryItem, error) {
	if limit < 1 || limit > MaxStoryItems {
		return nil, fmt.Errorf("story item limit must be between 1 and %d", MaxStoryItems)
	}
	rows, err := s.query(ctx, `SELECT id, source, title, link, COALESCE(summary, ''),
		published_at, created_at, COALESCE(llm_score, 0)
		FROM items
		WHERE trim(id) <> '' AND trim(source) <> '' AND trim(title) <> '' AND trim(link) <> ''
		ORDER BY `+itemDate+` DESC, id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query story items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]StoryItem, 0, limit)
	for rows.Next() {
		var (
			item      StoryItem
			published sql.NullTime
			firstSeen time.Time
		)
		if err := rows.Scan(&item.ID, &item.Source, &item.Title, &item.Link, &item.Summary,
			&published, &firstSeen, &item.Score); err != nil {
			return nil, fmt.Errorf("scan story item: %w", err)
		}
		if published.Valid {
			item.Published = utcTime(published.Time)
		} else {
			item.Published = utcTime(firstSeen)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate story items: %w", err)
	}
	return items, nil
}

// ReplaceStoryClusters atomically replaces clustering metadata. It never writes
// to the items table.
func (s *Store) ReplaceStoryClusters(ctx context.Context, clusters []StoryCluster) error {
	for _, cluster := range clusters {
		if err := validateStoryCluster(cluster); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin story cluster replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := s.txExec(ctx, tx, `DELETE FROM story_cluster_items`); err != nil {
		return fmt.Errorf("clear story cluster items: %w", err)
	}
	if _, err := s.txExec(ctx, tx, `DELETE FROM story_clusters`); err != nil {
		return fmt.Errorf("clear story clusters: %w", err)
	}

	clusterStmt, err := s.prepareTx(ctx, tx, `INSERT INTO story_clusters
		(id, representative_item_id, method, confidence, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare story cluster insert: %w", err)
	}
	defer func() { _ = clusterStmt.Close() }()
	itemStmt, err := s.prepareTx(ctx, tx, `INSERT INTO story_cluster_items
		(cluster_id, item_id, item_rank) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare story cluster item insert: %w", err)
	}
	defer func() { _ = itemStmt.Close() }()

	for _, cluster := range clusters {
		created := cluster.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}
		updated := cluster.UpdatedAt
		if updated.IsZero() {
			updated = time.Now()
		}
		if _, err := clusterStmt.ExecContext(ctx, cluster.ID, cluster.RepresentativeItemID,
			cluster.Method, cluster.Confidence, sqlTime(created), sqlTime(updated)); err != nil {
			return fmt.Errorf("insert story cluster %s: %w", cluster.ID, err)
		}
		for _, item := range cluster.Items {
			if _, err := itemStmt.ExecContext(ctx, cluster.ID, item.ItemID, item.Position); err != nil {
				return fmt.Errorf("insert story cluster item %s/%s: %w", cluster.ID, item.ItemID, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit story clusters: %w", err)
	}
	return nil
}

// StoryClusters returns every persisted story cluster.
func (s *Store) StoryClusters(ctx context.Context) ([]StoryCluster, error) {
	return s.queryStoryClusters(ctx, `SELECT id, representative_item_id, method, confidence,
		created_at, updated_at FROM story_clusters ORDER BY id ASC`)
}

// StoryClustersForItems returns the clusters that contain at least one of ids.
func (s *Store) StoryClustersForItems(ctx context.Context, ids []string) ([]StoryCluster, error) {
	ids = uniqueStoryIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.query(ctx, `SELECT DISTINCT cluster_id FROM story_cluster_items
		WHERE item_id IN (`+placeholders+`) ORDER BY cluster_id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("query story clusters for items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var clusterIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan story cluster id: %w", err)
		}
		clusterIDs = append(clusterIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate story cluster ids: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close story cluster ids: %w", err)
	}
	if len(clusterIDs) == 0 {
		return nil, nil
	}
	placeholders = strings.TrimSuffix(strings.Repeat("?,", len(clusterIDs)), ",")
	args = args[:0]
	for _, id := range clusterIDs {
		args = append(args, id)
	}
	return s.queryStoryClusters(ctx, `SELECT id, representative_item_id, method, confidence,
		created_at, updated_at FROM story_clusters WHERE id IN (`+placeholders+`) ORDER BY id ASC`,
		args...)
}

func (s *Store) queryStoryClusters(ctx context.Context, query string, args ...any) ([]StoryCluster, error) {
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query story clusters: %w", err)
	}
	defer func() { _ = rows.Close() }()

	clusters := make([]StoryCluster, 0)
	byID := make(map[string]int)
	for rows.Next() {
		var (
			cluster   StoryCluster
			createdAt time.Time
			updatedAt time.Time
		)
		if err := rows.Scan(&cluster.ID, &cluster.RepresentativeItemID, &cluster.Method,
			&cluster.Confidence, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan story cluster: %w", err)
		}
		cluster.CreatedAt = utcTime(createdAt)
		cluster.UpdatedAt = utcTime(updatedAt)
		byID[cluster.ID] = len(clusters)
		clusters = append(clusters, cluster)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate story clusters: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close story clusters: %w", err)
	}
	if len(clusters) == 0 {
		return clusters, nil
	}

	ids := make([]string, len(clusters))
	for i, cluster := range clusters {
		ids[i] = cluster.ID
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	itemArgs := make([]any, len(ids))
	for i, id := range ids {
		itemArgs[i] = id
	}
	itemRows, err := s.query(ctx, `SELECT cluster_id, item_id, item_rank
		FROM story_cluster_items WHERE cluster_id IN (`+placeholders+`)
		ORDER BY cluster_id ASC, item_rank ASC, item_id ASC`, itemArgs...)
	if err != nil {
		return nil, fmt.Errorf("query story cluster items: %w", err)
	}
	defer func() { _ = itemRows.Close() }()
	for itemRows.Next() {
		var (
			clusterID string
			item      StoryClusterItem
		)
		if err := itemRows.Scan(&clusterID, &item.ItemID, &item.Position); err != nil {
			return nil, fmt.Errorf("scan story cluster item: %w", err)
		}
		if idx, ok := byID[clusterID]; ok {
			clusters[idx].Items = append(clusters[idx].Items, item)
		}
	}
	if err := itemRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate story cluster items: %w", err)
	}
	return clusters, nil
}

func validateStoryCluster(cluster StoryCluster) error {
	if strings.TrimSpace(cluster.ID) == "" {
		return fmt.Errorf("story cluster id is required")
	}
	if strings.TrimSpace(cluster.RepresentativeItemID) == "" {
		return fmt.Errorf("story cluster %s representative is required", cluster.ID)
	}
	if len(cluster.Items) == 0 {
		return fmt.Errorf("story cluster %s has no items", cluster.ID)
	}
	found := false
	for _, item := range cluster.Items {
		if strings.TrimSpace(item.ItemID) == "" {
			return fmt.Errorf("story cluster %s has a blank item id", cluster.ID)
		}
		if item.ItemID == cluster.RepresentativeItemID {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("story cluster %s representative is not a member", cluster.ID)
	}
	return nil
}

func uniqueStoryIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
