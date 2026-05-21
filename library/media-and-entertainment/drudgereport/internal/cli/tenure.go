package cli

import (
	"database/sql"
	"fmt"
	"io"
	"time"

	"github.com/mvanhorn/printing-press-library/library/media-and-entertainment/drudgereport/internal/drudge"
	"github.com/mvanhorn/printing-press-library/library/media-and-entertainment/drudgereport/internal/store"
	"github.com/spf13/cobra"
)

// newTenureCmd returns the local splash-tenure command.
func newTenureCmd(flags *rootFlags) *cobra.Command {
	var history bool
	var limit int
	cmd := &cobra.Command{
		Use:         "tenure",
		Short:       "How long the current splash has been the splash; --history for longest-tenured list.",
		Example:     "  drudgereport-pp-cli tenure --history --json",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return nil
			}
			if limit < 0 {
				return usageErr(fmt.Errorf("--limit must be non-negative"))
			}
			if history && limit == 0 {
				limit = 10
			}

			ctx := cmd.Context()
			s, err := store.OpenWithContext(ctx, defaultDBPath("drudgereport-pp-cli"))
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer s.Close()
			if err := store.EnsureDrudgeSchema(ctx, s.DB()); err != nil {
				return fmt.Errorf("ensure drudge schema: %w", err)
			}

			empty, err := drudgeStoreEmpty(cmd, s.DB())
			if err != nil {
				return err
			}
			if empty {
				return emitDrudgeNoData(cmd.OutOrStdout(), flags)
			}

			if history {
				results, err := querySplashTenureHistory(cmd, s.DB(), limit)
				if err != nil {
					return err
				}
				return emitDrudgeLocal(cmd.OutOrStdout(), flags, results, func(w io.Writer) error {
					tw := newTabWriter(w)
					fmt.Fprintln(tw, "SECONDS\tFIRST SEEN\tLAST SEEN\tSTORY\tTITLE")
					for _, row := range results {
						fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%v\n", row["splash_tenure_seconds"], row["first_seen_at"], row["last_seen_at"], row["story_id"], row["title"])
					}
					return tw.Flush()
				})
			}

			result, err := queryCurrentSplashTenure(cmd, s.DB())
			if err != nil {
				return err
			}
			return emitDrudgeLocal(cmd.OutOrStdout(), flags, result, func(w io.Writer) error {
				if note, ok := result["note"]; ok {
					fmt.Fprintln(w, note)
					return nil
				}
				fmt.Fprintf(w, "%s\n%s\nfirst seen on splash: %v  tenure: %vs\n", bold(fmt.Sprint(result["title"])), result["url"], result["splash_first_seen_at"], result["splash_tenure_seconds"])
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&history, "history", false, "Show longest-tenured splashes over local history")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum history rows (default 10 with --history)")
	return cmd
}

func queryCurrentSplashTenure(cmd *cobra.Command, db *sql.DB) (map[string]any, error) {
	var storyID, title, url string
	err := db.QueryRowContext(cmd.Context(),
		`SELECT story_id, title, url
		 FROM drudge_story
		 WHERE snapshot_id = (SELECT snapshot_id FROM drudge_snapshot ORDER BY captured_at DESC LIMIT 1)
		   AND slot = ?
		 ORDER BY slot_index
		 LIMIT 1`,
		string(drudge.SlotSplash),
	).Scan(&storyID, &title, &url)
	if err == sql.ErrNoRows {
		return map[string]any{"splash_tenure_seconds": int64(0), "note": "No splash observed in latest snapshot."}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query current splash: %w", err)
	}

	// PATCH(greptile-2026-05-21:tenure-contiguous): current-splash tenure must
	// reflect the active contiguous splash run, not the all-time first sighting.
	// A story that was previously demoted from splash and later re-promoted
	// should report seconds since the most recent promotion. The earliest
	// captured_at of the current contiguous run is the smallest captured_at
	// strictly after the most recent non-splash row for this story_id (or
	// the epoch when the story has never been off splash, in which case the
	// query collapses to the all-time MIN).
	var firstSeen sql.NullString
	if err := db.QueryRowContext(cmd.Context(),
		`SELECT MIN(captured_at) FROM drudge_story
		 WHERE story_id = ?
		   AND slot = ?
		   AND captured_at > COALESCE((
		     SELECT MAX(captured_at) FROM drudge_story
		     WHERE story_id = ? AND slot != ?
		   ), '1970-01-01T00:00:00Z')`,
		storyID, string(drudge.SlotSplash), storyID, string(drudge.SlotSplash),
	).Scan(&firstSeen); err != nil {
		return nil, fmt.Errorf("query splash first seen: %w", err)
	}
	tenureSeconds := int64(0)
	if firstSeen.Valid && firstSeen.String != "" {
		var err error
		tenureSeconds, err = secondsSince(firstSeen.String, time.Now().UTC())
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"story_id":                 storyID,
		"title":                    title,
		"url":                      url,
		"splash_tenure_seconds":    tenureSeconds,
		"splash_first_seen_at":     nullStringText(firstSeen),
		"splash_first_seen_at_utc": nullStringText(firstSeen),
	}, nil
}

func querySplashTenureHistory(cmd *cobra.Command, db *sql.DB, limit int) ([]map[string]any, error) {
	// PATCH(greptile-2026-05-21:tenure-contiguous): the all-time MIN/MAX
	// over every splash row for a story overstates tenure when a story has
	// been demoted from splash and re-promoted. The leaderboard must rank
	// by the longest CONTIGUOUS splash run per story, not the gross span.
	//
	// Pattern: tag every splash row with its run-start marker (the MAX
	// non-splash captured_at for the same story_id that is strictly
	// earlier than this splash row, or the epoch if the story has never
	// been off splash). Splash rows sharing the same marker belong to the
	// same contiguous run. Then GROUP BY (story_id, marker) to get
	// per-run MIN/MAX, take ROW_NUMBER() OVER (PARTITION BY story_id
	// ORDER BY run length DESC) = 1 per story, and rank.
	rows, err := db.QueryContext(cmd.Context(),
		`WITH splash_rows AS (
			SELECT story_id, captured_at
			FROM drudge_story
			WHERE slot = ?
		),
		run_marked AS (
			SELECT
				sr.story_id,
				sr.captured_at,
				COALESCE((
					SELECT MAX(captured_at) FROM drudge_story
					WHERE story_id = sr.story_id
					  AND slot != ?
					  AND captured_at < sr.captured_at
				), '1970-01-01T00:00:00Z') AS run_marker
			FROM splash_rows sr
		),
		runs AS (
			SELECT
				story_id,
				run_marker,
				MIN(captured_at) AS first_seen_at,
				MAX(captured_at) AS last_seen_at,
				(strftime('%s', MAX(captured_at)) - strftime('%s', MIN(captured_at))) AS run_length_s
			FROM run_marked
			GROUP BY story_id, run_marker
		),
		ranked AS (
			SELECT
				story_id,
				first_seen_at,
				last_seen_at,
				run_length_s,
				ROW_NUMBER() OVER (PARTITION BY story_id ORDER BY run_length_s DESC, first_seen_at ASC) AS rn
			FROM runs
		)
		SELECT
			r.story_id,
			COALESCE(s.title, '') AS title,
			COALESCE(s.url, '') AS url,
			r.first_seen_at,
			r.last_seen_at
		FROM ranked r
		LEFT JOIN drudge_story s
			ON s.story_id = r.story_id
		   AND s.captured_at = (
				SELECT MAX(captured_at) FROM drudge_story
				WHERE story_id = r.story_id
		   )
		WHERE r.rn = 1
		ORDER BY r.run_length_s DESC, r.first_seen_at ASC
		LIMIT ?`,
		string(drudge.SlotSplash), string(drudge.SlotSplash), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query splash history: %w", err)
	}
	defer rows.Close()

	results := make([]map[string]any, 0)
	for rows.Next() {
		var storyID, title, url string
		var firstSeen, lastSeen sql.NullString
		if err := rows.Scan(&storyID, &title, &url, &firstSeen, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan splash history: %w", err)
		}
		tenureSeconds := int64(0)
		if firstSeen.Valid && lastSeen.Valid {
			var err error
			tenureSeconds, err = secondsBetween(firstSeen.String, lastSeen.String)
			if err != nil {
				return nil, err
			}
		}
		results = append(results, map[string]any{
			"story_id":                 storyID,
			"title":                    title,
			"url":                      url,
			"first_seen_at":            nullStringText(firstSeen),
			"last_seen_at":             nullStringText(lastSeen),
			"splash_tenure_seconds":    tenureSeconds,
			"splash_first_seen_at_utc": nullStringText(firstSeen),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate splash history: %w", err)
	}
	return results, nil
}
