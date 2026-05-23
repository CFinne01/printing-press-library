// Copyright 2026 cfinney. Licensed under Apache-2.0. See LICENSE.
// Hand-written novel feature for pangolin-pp-cli.

package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// restoreOrder is the apply order: parents (orgs, idp, users) before
// dependents (sites, resources, targets) before bindings (role assignments).
var restoreOrder = []string{
	"orgs", "idp", "users",
	"sites", "resources", "site_resources",
	"target", "client", "role", "certificate", "domains",
	"org_users", "org_roles", "org_idp", "org_domains",
}

// restorePathFor maps a backup resource_type to the POST path used to
// (re-)create that record against a live Pangolin host. Returns "" when the
// resource_type is not restorable through the integration API.
func restorePathFor(rt string) string {
	m := map[string]string{
		"orgs":      "/org",
		"idp":       "/idp",
		"sites":     "/site",
		"resources": "/resource",
		"target":    "/target",
		"client":    "/client",
		"role":      "/role",
	}
	return m[rt]
}

func newRestoreCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restore [backup.json]",
		Short: "Re-apply a backup against a fresh Pangolin install in dependency order.",
		Long: `restore reads a backup file produced by 'backup' and POSTs each record back
to the live Pangolin host in correct dependency order (orgs first, then sites,
then resources, then bindings).

Always run with --dry-run first to preview the apply plan. The command does
NOT delete existing records — it only creates the entries listed in the file.
A real restore against a non-empty host will surface duplicate-ID errors per
record and continue with the rest.`,
		Example: "  pangolin-pp-cli restore pangolin-backup.json --dry-run",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,4",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			path := args[0]
			raw, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			var snap backupFile
			if err := json.Unmarshal(raw, &snap); err != nil {
				return fmt.Errorf("parsing backup: %w", err)
			}
			if snap.Schema != "pangolin-pp-cli/backup" {
				return fmt.Errorf("file %s is not a pangolin-pp-cli backup (schema=%q)", path, snap.Schema)
			}

			plan := []map[string]any{}
			for _, rt := range restoreOrder {
				items, ok := snap.ResourceSets[rt]
				if !ok || len(items) == 0 {
					continue
				}
				postPath := restorePathFor(rt)
				if postPath == "" {
					continue
				}
				plan = append(plan, map[string]any{
					"resource_type": rt,
					"post_path":     postPath,
					"records":       len(items),
				})
			}

			if flags.dryRun {
				out := map[string]any{
					"dry_run":      true,
					"backup_file":  path,
					"generated_at": snap.GeneratedAt,
					"plan":         plan,
				}
				return printJSONFiltered(cmd.OutOrStdout(), out, flags)
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}

			applied := 0
			errored := 0
			for _, step := range plan {
				rt := step["resource_type"].(string)
				postPath := step["post_path"].(string)
				items := snap.ResourceSets[rt]
				for _, item := range items {
					var body any
					if json.Unmarshal(item, &body) != nil {
						errored++
						continue
					}
					if _, _, perr := c.Post(cmd.Context(), postPath, body); perr != nil {
						errored++
						fmt.Fprintf(cmd.ErrOrStderr(), "warn: POST %s failed: %v\n", postPath, perr)
						continue
					}
					applied++
				}
			}

			result := map[string]any{
				"backup_file": path,
				"applied":     applied,
				"errored":     errored,
			}
			if errored > 0 {
				_ = printJSONFiltered(cmd.OutOrStdout(), result, flags)
				return fmt.Errorf("restore completed with %d errors", errored)
			}
			return printJSONFiltered(cmd.OutOrStdout(), result, flags)
		},
	}
	return cmd
}
