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

	// Field definitions — pair with a Fleet gjson path below.
	fields := []snipe.FieldDef{
		{Name: "Fleet: Host ID", Element: "text", Format: "NUMERIC", HelpText: "Fleet's internal host ID"},
		{Name: "Fleet: Hostname", Element: "text", Format: "ANY", HelpText: "Hostname reported by osquery"},
		{Name: "Fleet: Platform", Element: "text", Format: "ANY", HelpText: "OS platform (darwin, windows, linux, chrome, ios, ipados)"},
		{Name: "Fleet: OS Version", Element: "text", Format: "ANY", HelpText: "Full OS version string"},
		{Name: "Fleet: OS Build", Element: "text", Format: "ANY", HelpText: "OS build identifier"},
		{Name: "Fleet: Status", Element: "radio", Format: "ANY", HelpText: "Online status as last reported by Fleet", FieldValues: "online\noffline\nmissing\nnew"},
		{Name: "Fleet: Primary IP", Element: "text", Format: "IP", HelpText: "Primary private IP"},
		{Name: "Fleet: Primary MAC", Element: "text", Format: "MAC", HelpText: "Primary MAC address"},
		{Name: "Fleet: Public IP", Element: "text", Format: "ANY", HelpText: "Public IP last seen by Fleet"},
		{Name: "Fleet: CPU Brand", Element: "text", Format: "ANY", HelpText: "CPU brand string"},
		{Name: "Fleet: Memory (bytes)", Element: "text", Format: "NUMERIC", HelpText: "RAM in bytes"},
		{Name: "Fleet: Disk Free (GiB)", Element: "text", Format: "NUMERIC", HelpText: "Free disk space in GiB"},
		{Name: "Fleet: Disk Total (GiB)", Element: "text", Format: "NUMERIC", HelpText: "Total disk space in GiB"},
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

	// field name -> Fleet gjson path. Adjust if you change the field list above.
	pathByField := map[string]string{
		"Fleet: Host ID":          "id",
		"Fleet: Hostname":         "hostname",
		"Fleet: Platform":         "platform",
		"Fleet: OS Version":       "os_version",
		"Fleet: OS Build":         "build",
		"Fleet: Status":           "status",
		"Fleet: Primary IP":       "primary_ip",
		"Fleet: Primary MAC":      "primary_mac",
		"Fleet: Public IP":        "public_ip",
		"Fleet: CPU Brand":        "cpu_brand",
		"Fleet: Memory (bytes)":   "memory",
		"Fleet: Disk Free (GiB)":  "gigs_disk_space_available",
		"Fleet: Disk Total (GiB)": "gigs_total_disk_space",
		"Fleet: Disk Encryption":  "disk_encryption_enabled",
		"Fleet: MDM Enrollment":   "mdm.enrollment_status",
		"Fleet: MDM Server":       "mdm.server_url",
		"Fleet: MDM Name":         "mdm.name",
		"Fleet: Team":             "team_name",
		"Fleet: Last Enrolled At": "last_enrolled_at",
		"Fleet: Last Seen":        "seen_time",
		"Fleet: Osquery Version":  "osquery_version",
	}

	mapping := make(map[string]string, len(created))
	replace := make(map[string]bool, len(created))
	for name, dbCol := range created {
		if path, ok := pathByField[name]; ok && dbCol != "" {
			mapping[dbCol] = path
			replace[path] = true
		}
	}

	if err := config.MergeFieldMapping(ConfigFile, mapping, replace); err != nil {
		log.Warnf("could not save field mappings to %s: %v", ConfigFile, err)
		fmt.Println("\nAdd these to your settings.yaml sync.field_mapping manually:")
		for dbCol, path := range mapping {
			fmt.Printf("    %s: %s\n", dbCol, path)
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
