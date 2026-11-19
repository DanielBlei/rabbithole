// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

var tagsCmd = &cobra.Command{
	Use:   "tags",
	Short: "Inspect and curate the content-tag vocabulary",
}

func init() {
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List canonical content tags and their aliases",
		Args:  cobra.NoArgs,
		RunE:  runTagsList,
	}
	aliasCmd := &cobra.Command{
		Use:   "alias <alias> <canonical>",
		Short: "Resolve an alternate spelling to one canonical content tag",
		Args:  cobra.ExactArgs(2),
		RunE:  runTagsAlias,
	}
	tagsCmd.AddCommand(listCmd, aliasCmd)
	rootCmd.AddCommand(tagsCmd)
}

func runTagsList(cmd *cobra.Command, _ []string) error {
	return withStore(cmd, func(ctx context.Context, db *store.Store, _ config.Source) error {
		tags, err := db.ListTags(ctx)
		if err != nil {
			return err
		}
		if len(tags) == 0 {
			fmt.Println("No content tags yet.")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(w, "SLUG\tLABEL\tALIASES"); err != nil {
			return err
		}
		for _, tag := range tags {
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n",
				tag.Slug, tag.Label, strings.Join(tag.Aliases, ", ")); err != nil {
				return err
			}
		}
		return w.Flush()
	})
}

func runTagsAlias(cmd *cobra.Command, args []string) error {
	return withStore(cmd, func(ctx context.Context, db *store.Store, _ config.Source) error {
		if err := db.AddTagAlias(ctx, args[0], args[1]); err != nil {
			return err
		}
		fmt.Printf("Alias %q now resolves to %q.\n", args[0], args[1])
		return nil
	})
}
