package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vocoder/coldarr/internal/mover"
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
			report.Warnings(os.Stdout, inv.Warnings)

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

			lock, err := mover.AcquireLock(filepath.Dir(configPath))
			if err != nil {
				return err
			}
			defer lock.Release()

			fmt.Println("\nApplying - one move at a time per destination volume, so this can take a while for large plans:")
			progress := e.Movers().Apply(plan, inv.VolumeOf())
			printProgressUntilDone(progress)

			snap := progress.Snapshot()
			moved := snap.Moved()
			failed := snap.Failed()

			fmt.Printf("\nMoved %d item(s).\n", len(moved))
			if len(failed) > 0 {
				fmt.Printf("Failed to move %d item(s):\n", len(failed))
				for _, f := range failed {
					fmt.Printf("  - %s: %s\n", f.Entry.Item.Title, f.Err)
				}
			}

			if len(moved) > 0 {
				if jf := e.JellyfinClient(); jf != nil {
					fmt.Println("Triggering Jellyfin library refresh...")
					if err := jf.RefreshLibrary(); err != nil {
						fmt.Fprintf(os.Stderr, "warning: jellyfin refresh failed: %v\n", err)
					}
				}
			}

			if len(failed) > 0 {
				return fmt.Errorf("%d move(s) failed", len(failed))
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// printProgressUntilDone polls progress and prints a line every time an
// item's status changes, until the whole run finishes.
func printProgressUntilDone(progress *mover.Progress) {
	last := map[int]mover.MoveStatus{}
	done := make(chan struct{})
	go func() {
		progress.Wait()
		close(done)
	}()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	printChanges := func() {
		for i, e := range progress.Snapshot().Entries {
			if last[i] == e.Status {
				continue
			}
			last[i] = e.Status
			switch e.Status {
			case mover.StatusMoving:
				fmt.Printf("  moving:  %s -> %s\n", e.Entry.Item.Title, e.Entry.ToPath)
			case mover.StatusDone:
				fmt.Printf("  done:    %s\n", e.Entry.Item.Title)
			case mover.StatusFailed:
				fmt.Printf("  failed:  %s: %s\n", e.Entry.Item.Title, e.Err)
			}
		}
	}

	for {
		select {
		case <-done:
			printChanges()
			return
		case <-ticker.C:
			printChanges()
		}
	}
}
