package main

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/vocoder/coldarr/internal/report"
)

func newReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "report",
		Short: "Show tier usage and scored inventory - read-only, makes no changes",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadEngine()
			if err != nil {
				return err
			}

			inv, err := e.BuildInventory(time.Now())
			if err != nil {
				return err
			}

			report.TierUsage(os.Stdout, inv)
			report.Summary(os.Stdout, inv, 20)
			return nil
		},
	}
}
