package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/CampusTech/fleet2snipe/config"
	"github.com/CampusTech/fleet2snipe/fleetapi"
	"github.com/CampusTech/fleet2snipe/images"
	f2sync "github.com/CampusTech/fleet2snipe/sync"
)

// NewSyncCmd builds the `sync` subcommand: full reconciliation of Fleet hosts
// into Snipe-IT.
func NewSyncCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sync",
		Short: "Sync all Fleet hosts into Snipe-IT",
		Long:  "Lists every host in Fleet (paged) and reconciles each one into Snipe-IT — creating new assets or updating existing ones based on hardware serial.",
		RunE:  runSync,
	}
	c.Flags().Bool("force", false, "Ignore timestamps, always update")
	c.Flags().String("serial", "", "Sync a single host by hardware serial (implies --force)")
	c.Flags().String("identifier", "", "Sync a single host by uuid/hostname/serial/node_key (implies --force)")
	c.Flags().Bool("update-only", false, "Only update existing Snipe-IT assets, never create new ones")
	c.Flags().Bool("use-cache", false, "Read hosts from .cache/hosts.json instead of the Fleet API")
	c.Flags().Bool("populate-software", false, "Include software inventory (no vuln details) — heavier, only enable when mapping software fields")
	return c
}

func runSync(cmd *cobra.Command, _ []string) error {
	applyBoolFlag(cmd, "force", &Cfg.Sync.Force)
	applyBoolFlag(cmd, "update-only", &Cfg.Sync.UpdateOnly)
	applyBoolFlag(cmd, "use-cache", &Cfg.Sync.UseCache)
	if cmd.Flags().Changed("populate-software") {
		v, _ := cmd.Flags().GetBool("populate-software")
		Cfg.Fleet.PopulateSoftware = v
	}

	serial, _ := cmd.Flags().GetString("serial")
	identifier, _ := cmd.Flags().GetString("identifier")
	if serial != "" || identifier != "" {
		Cfg.Sync.Force = true
	}

	if err := Cfg.Validate(); err != nil {
		return err
	}

	// Auto-enable populate_* on the list endpoint when their corresponding
	// mappings are set — the sync engine can't read what Fleet doesn't return.
	if len(Cfg.Sync.PolicyMapping) > 0 {
		Cfg.Fleet.PopulatePolicies = true
	}
	if len(Cfg.Sync.LabelMapping) > 0 || Cfg.Sync.LabelsField != "" {
		Cfg.Fleet.PopulateLabels = true
	}

	if Cfg.Sync.DryRun {
		log.Info("running in DRY RUN mode — no changes will be made")
	}

	ctx, cancel := contextWithSignal()
	defer cancel()

	var fleetClient *fleetapi.Client
	if !Cfg.Sync.UseCache || serial != "" || identifier != "" {
		var err error
		fleetClient, err = newFleetClient()
		if err != nil {
			return err
		}
	}

	snipeClient, err := newSnipeClient()
	if err != nil {
		return err
	}

	engine := f2sync.NewEngine(fleetClient, snipeClient, Cfg)
	if Cfg.Sync.ModelImages {
		engine.WithImages(images.NewFetcher())
	}
	if err := engine.Warm(ctx); err != nil {
		return err
	}

	// Single-host path.
	if serial != "" || identifier != "" {
		key := identifier
		if key == "" {
			key = serial
		}
		host, err := fleetClient.HostByIdentifier(ctx, key)
		if err != nil {
			return fmt.Errorf("looking up host %q: %w", key, err)
		}
		if err := engine.SyncHost(ctx, *host); err != nil {
			return err
		}
		printStats(engine.Stats())
		return nil
	}

	hosts, err := loadHosts(ctx, fleetClient, Cfg)
	if err != nil {
		return err
	}
	log.Infof("loaded %d hosts from Fleet", len(hosts))

	if _, err := engine.SyncAll(ctx, hosts); err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}
	printStats(engine.Stats())
	return nil
}

// loadHosts returns all Fleet hosts, either from cache or the API. When using
// the API and a cache dir is configured, the fetched list is written to
// .cache/hosts.json for offline reuse.
func loadHosts(ctx context.Context, client *fleetapi.Client, cfg *config.Config) ([]fleetapi.Host, error) {
	cacheDir := cfg.Sync.CacheDir
	if cacheDir == "" {
		cacheDir = ".cache"
	}
	cachePath := filepath.Join(cacheDir, "hosts.json")

	if cfg.Sync.UseCache {
		data, err := os.ReadFile(cachePath)
		if err != nil {
			return nil, fmt.Errorf("reading cache %s: %w (run without --use-cache first)", cachePath, err)
		}
		return f2sync.DeserializeHosts(data)
	}

	hosts, err := client.ListAllHosts(ctx, fleetapi.ListHostsOptions{
		PerPage:          cfg.Fleet.EffectivePerPage(),
		TeamID:           cfg.Fleet.TeamID,
		PopulateSoftware: cfg.Fleet.PopulateSoftware,
		PopulateLabels:   cfg.Fleet.PopulateLabels,
		PopulateUsers:    cfg.Fleet.PopulateUsers,
		PopulatePolicies: cfg.Fleet.PopulatePolicies,
	})
	if err != nil {
		return nil, fmt.Errorf("fetching hosts: %w", err)
	}

	// Best-effort cache write.
	if err := os.MkdirAll(cacheDir, 0o755); err == nil {
		if data, mErr := f2sync.SerializeHosts(hosts); mErr == nil {
			_ = os.WriteFile(cachePath, data, 0o644)
		}
	}
	return hosts, nil
}

func printStats(s f2sync.Stats) {
	fmt.Printf("\nSync Results:\n")
	fmt.Printf("  Total hosts processed: %d\n", s.Total)
	fmt.Printf("  Assets created:        %d\n", s.Created)
	fmt.Printf("  Assets updated:        %d\n", s.Updated)
	fmt.Printf("  Assets skipped:        %d\n", s.Skipped)
	fmt.Printf("  Errors:                %d\n", s.Errors)
	fmt.Printf("  New models:            %d\n", s.ModelsCreated)
	fmt.Printf("  New manufacturers:     %d\n", s.ManufacturersNew)
	if s.CheckoutsApplied+s.CheckoutsSkipped > 0 {
		fmt.Printf("  Checkouts applied:     %d\n", s.CheckoutsApplied)
		fmt.Printf("  Checkouts skipped:     %d\n", s.CheckoutsSkipped)
	}
}
