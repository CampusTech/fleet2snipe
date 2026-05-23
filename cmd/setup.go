package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/CampusTech/fleet2snipe/config"
	"github.com/CampusTech/fleet2snipe/snipe"
)

// NewSetupCmd returns the `setup` subcommand: idempotent bootstrap of the
// Snipe-IT custom fields fleet2snipe relies on, plus updates the local config
// file with the generated field_mapping entries.
func NewSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Create the Fleet-related custom fields in Snipe-IT",
		Long:  "Creates (or updates) custom fields in Snipe-IT, associates them with the configured fieldset, and writes the resulting db_column_name → fleet gjson path mappings back into your settings.yaml.\n\nYou must create the fieldset, manufacturers, model categories, and default status label manually before running setup.",
		RunE:  runSetup,
	}
}

func runSetup(cmd *cobra.Command, _ []string) error {
	if err := Cfg.ValidateSnipeIT(); err != nil {
		return err
	}
	fieldsetIDs := Cfg.SnipeIT.AllFieldsetIDs()
	if len(fieldsetIDs) == 0 {
		return fmt.Errorf("at least one fieldset is required: set snipe_it.custom_fieldset_id and/or snipe_it.fieldset_ids in %s before running setup", ConfigFile)
	}

	if Cfg.Sync.DryRun {
		log.Info("running in DRY RUN mode — no changes will be made")
	}

	_, cancel := contextWithSignal()
	defer cancel()

	client, err := newSnipeClient()
	if err != nil {
		return err
	}

	// Field definitions — pair with a Fleet gjson path + optional default
	// transform in entryByField below. The (GB) / (GiB) suffixes reflect the
	// transformed unit so the field name doesn't lie about its contents.
	fields := []snipe.FieldDef{
		{Name: "Fleet: Host ID", Element: "text", Format: "NUMERIC", HelpText: "Fleet's internal host ID"},
		{Name: "Fleet: Hostname", Element: "text", Format: "ANY", HelpText: "Hostname reported by osquery"},
		{Name: "Fleet: Platform", Element: "text", Format: "ANY", HelpText: "OS platform (darwin, windows, linux, chrome, ios, ipados)"},
		{Name: "Fleet: OS Version", Element: "text", Format: "ANY", HelpText: "Full OS version string"},
		{Name: "Fleet: OS Build", Element: "text", Format: "ANY", HelpText: "OS build identifier"},
		{Name: "Fleet: Status", Element: "radio", Format: "ANY", HelpText: "Online status as last reported by Fleet", FieldValues: "online\noffline\nmissing\nnew"},
		{Name: "Fleet: Primary IP", Element: "text", Format: "IP", HelpText: "Primary private IP"},
		{Name: "Fleet: Primary MAC", Element: "text", Format: "MAC", HelpText: "Primary MAC address (lowercase, colon-separated)"},
		{Name: "Fleet: Public IP", Element: "text", Format: "ANY", HelpText: "Public IP last seen by Fleet"},
		{Name: "Fleet: CPU Brand", Element: "text", Format: "ANY", HelpText: "CPU brand string"},
		{Name: "Fleet: Memory (GB)", Element: "text", Format: "NUMERIC", HelpText: "RAM, binary GiB (the 'GB' figure About This Mac shows for RAM)"},
		{Name: "Fleet: Disk Free (GB)", Element: "text", Format: "NUMERIC", HelpText: "Free disk space, decimal GB (matches marketed disk capacity)"},
		{Name: "Fleet: Disk Total (GB)", Element: "text", Format: "NUMERIC", HelpText: "Total disk space, decimal GB"},
		{Name: "Fleet: Disk Encryption", Element: "listbox", Format: "BOOLEAN", HelpText: "Whether full-disk encryption is enabled (detail endpoint only)", FieldValues: "true\nfalse"},
		{Name: "Fleet: MDM Enrollment", Element: "text", Format: "ANY", HelpText: "MDM enrollment status (e.g. 'On (manual)')"},
		{Name: "Fleet: MDM Server", Element: "text", Format: "URL", HelpText: "MDM server URL"},
		{Name: "Fleet: MDM Name", Element: "text", Format: "ANY", HelpText: "MDM solution name"},
		{Name: "Fleet: Team", Element: "text", Format: "ANY", HelpText: "Fleet team this host is assigned to"},
		{Name: "Fleet: Last Enrolled At", Element: "text", Format: "ANY", HelpText: "When the host enrolled in Fleet"},
		{Name: "Fleet: Last Seen", Element: "text", Format: "ANY", HelpText: "Last contact with Fleet"},
		{Name: "Fleet: Osquery Version", Element: "text", Format: "ANY", HelpText: "Osquery agent version"},
	}

	log.WithField("fieldsets", fieldsetIDs).Info("creating custom fields in Snipe-IT...")
	created, err := client.SetupFields(fieldsetIDs, fields)
	if err != nil {
		return fmt.Errorf("setting up fields: %w", err)
	}

	// field name -> (gjson path, optional transform). Memory uses bytes_to_gib
	// (binary, matches About This Mac); disk uses gib_to_gb (decimal, matches
	// marketed capacity); MAC uses mac_colons (canonical lowercase form).
	entryByField := map[string]config.FieldMappingEntry{
		"Fleet: Host ID":          {Path: "id"},
		"Fleet: Hostname":         {Path: "hostname"},
		"Fleet: Platform":         {Path: "platform"},
		"Fleet: OS Version":       {Path: "os_version"},
		"Fleet: OS Build":         {Path: "build"},
		"Fleet: Status":           {Path: "status"},
		"Fleet: Primary IP":       {Path: "primary_ip"},
		"Fleet: Primary MAC":      {Path: "primary_mac", Transform: "mac_colons"},
		"Fleet: Public IP":        {Path: "public_ip"},
		"Fleet: CPU Brand":        {Path: "cpu_brand"},
		"Fleet: Memory (GB)":      {Path: "memory", Transform: "bytes_to_gib"},
		"Fleet: Disk Free (GB)":   {Path: "gigs_disk_space_available", Transform: "gib_to_gb"},
		"Fleet: Disk Total (GB)":  {Path: "gigs_total_disk_space", Transform: "gib_to_gb"},
		"Fleet: Disk Encryption":  {Path: "disk_encryption_enabled"},
		"Fleet: MDM Enrollment":   {Path: "mdm.enrollment_status"},
		"Fleet: MDM Server":       {Path: "mdm.server_url"},
		"Fleet: MDM Name":         {Path: "mdm.name"},
		"Fleet: Team":             {Path: "team_name"},
		"Fleet: Last Enrolled At": {Path: "last_enrolled_at"},
		"Fleet: Last Seen":        {Path: "seen_time"},
		"Fleet: Osquery Version":  {Path: "osquery_version"},
	}

	mapping := make(map[string]config.FieldMappingEntry, len(created))
	replace := make(map[string]bool, len(created))
	for name, dbCol := range created {
		if entry, ok := entryByField[name]; ok && dbCol != "" {
			mapping[dbCol] = entry
			replace[entry.Path] = true
		}
	}

	if err := config.MergeFieldMapping(ConfigFile, mapping, replace); err != nil {
		log.Warnf("could not save field mappings to %s: %v", ConfigFile, err)
		fmt.Println("\nAdd these to your settings.yaml sync.field_mapping manually:")
		for dbCol, entry := range mapping {
			if entry.Transform == "" {
				fmt.Printf("    %s: %s\n", dbCol, entry.Path)
			} else {
				fmt.Printf("    %s:\n      path: %s\n      transform: %s\n", dbCol, entry.Path, entry.Transform)
			}
		}
	} else {
		fmt.Printf("\nField mappings saved to %s\n", ConfigFile)
	}

	fmt.Println("\nCustom fields created/updated:")
	for name, dbCol := range created {
		fmt.Printf("  %-30s  %s\n", name, dbCol)
	}
	return nil
}
