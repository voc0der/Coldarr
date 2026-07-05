package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vocoder/coldarr/internal/report"
)

func newApplyCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Build a move plan and execute it through Radarr/Sonarr, after confirmation",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadEngine()
			if err != nil {
				return err
			}

			now := time.Now()
			inv, err := e.BuildInventory(now)
			if err != nil {
				return err
			}

			report.TierUsage(os.Stdout, inv)

			plan, err := e.BuildPlan(inv, now)
			if err != nil {
				return err
			}

			report.Plan(os.Stdout, plan)
			report.ProjectedUsage(os.Stdout, inv.UsableUsage(), plan.FinalUsage, inv.Tiers)

			if len(plan.Entries) == 0 {
				fmt.Println("\nNothing to do.")
				return nil
			}

			if !yes {
				fmt.Printf("\nProceed with %d move(s)? [y/N]: ", len(plan.Entries))
				reader := bufio.NewReader(os.Stdin)
				line, _ := reader.ReadString('\n')
				answer := strings.ToLower(strings.TrimSpace(line))
				if answer != "y" && answer != "yes" {
					fmt.Println("Aborted - no changes made.")
					return nil
				}
			}

			result, err := e.Movers().Apply(plan, now)
			if err != nil {
				return err
			}

			fmt.Printf("\nMoved %d item(s).\n", len(result.Moved))
			if len(result.Failed) > 0 {
				fmt.Printf("Failed to move %d item(s):\n", len(result.Failed))
				for _, f := range result.Failed {
					fmt.Printf("  - %s: %v\n", f.Entry.Item.Title, f.Err)
				}
			}

			if len(result.Moved) > 0 {
				if jf := e.JellyfinClient(); jf != nil {
					fmt.Println("Triggering Jellyfin library refresh...")
					if err := jf.RefreshLibrary(); err != nil {
						fmt.Fprintf(os.Stderr, "warning: jellyfin refresh failed: %v\n", err)
					}
				}
			}

			if len(result.Failed) > 0 {
				return fmt.Errorf("%d move(s) failed", len(result.Failed))
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}
