package main

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/vocoder/coldarr/internal/report"
)

func newPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Build and print a proposed move plan - dry run, makes no changes",
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
			return nil
		},
	}
}
