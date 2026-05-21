package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewTestCmd returns the `test` subcommand — verifies API connectivity to
// both Fleet and Snipe-IT.
func NewTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Test connectivity to Fleet and Snipe-IT",
		Long:  "Pings both Fleet and Snipe-IT with the configured credentials and prints versions / basic counts.",
		RunE:  runTest,
	}
}

func runTest(_ *cobra.Command, _ []string) error {
	if err := Cfg.Validate(); err != nil {
		return err
	}

	ctx, cancel := contextWithSignal()
	defer cancel()

	fleetClient, err := newFleetClient()
	if err != nil {
		return err
	}
	v, err := fleetClient.Version(ctx)
	if err != nil {
		return fmt.Errorf("fleet connection failed: %w", err)
	}
	fmt.Printf("Fleet OK — version %s (branch %s, revision %s)\n", v.Version, v.Branch, v.Revision)

	snipeClient, err := newSnipeClient()
	if err != nil {
		return err
	}
	if err := snipeClient.Ping(ctx); err != nil {
		return fmt.Errorf("snipe-it connection failed: %w", err)
	}
	fmt.Println("Snipe-IT OK")
	return nil
}
