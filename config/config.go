// Package config loads fleet2snipe configuration from YAML, applies env-var
// overrides, and exposes helpers used by the setup command to merge field
// mappings back into the config file without clobbering comments.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level config structure.
type Config struct {
	Fleet   FleetConfig   `yaml:"fleet"`
	SnipeIT SnipeITConfig `yaml:"snipe_it"`
	Sync    SyncConfig    `yaml:"sync"`
	Webhook WebhookConfig `yaml:"webhook"`
}

// FleetConfig holds Fleet (fleetdm.com) API settings.
type FleetConfig struct {
	URL              string `yaml:"url"`               // e.g. https://fleet.example.com
	Token            string `yaml:"token"`             // API-only user bearer token
	InsecureTLS      bool   `yaml:"insecure_tls"`      // skip TLS verification
	TeamID           int    `yaml:"team_id"`           // optional team filter (Premium)
	PerPage          int    `yaml:"per_page"`          // hosts page size (default 1000)
	PopulateSoftware bool   `yaml:"populate_software"` // include software inventory (without vuln details)
	PopulateLabels   bool   `yaml:"populate_labels"`
	PopulateUsers    bool   `yaml:"populate_users"`
	PopulatePolicies bool   `yaml:"populate_policies"` // include policy results (auto-enabled when sync.policy_mapping is set)
}

// SnipeITConfig holds Snipe-IT API settings.
type SnipeITConfig struct {
	URL             string `yaml:"url"`
	APIKey          string `yaml:"api_key"`
	DefaultStatusID int    `yaml:"default_status_id"` // status assigned to newly created assets
	// CustomFieldsetID is the default fieldset attached to auto-created models,
	// used when no per-platform fieldset is configured for the host's platform.
	CustomFieldsetID int `yaml:"custom_fieldset_id"`
	// FieldsetIDs maps a Fleet platform (e.g. "darwin", "windows", "linux",
	// "chrome", "ios", "ipados") to a Snipe-IT fieldset ID. Falls back to
	// CustomFieldsetID when the platform isn't mapped — matches the
	// computer_custom_fieldset_id / mobile_custom_fieldset_id pattern in
	// jamf2snipe but generalised to N platforms.
	FieldsetIDs map[string]int `yaml:"fieldset_ids"`
	// ManufacturerIDs maps a hardware_vendor string (lowercased, e.g. "apple inc.") to a
	// Snipe-IT manufacturer ID. The sync engine ensureManufacturer falls back to
	// auto-create when a vendor is not mapped.
	ManufacturerIDs map[string]int `yaml:"manufacturer_ids"`
	// CategoryIDs maps a Fleet platform string (e.g. "darwin", "windows", "linux",
	// "chrome", "ios", "ipados") to a Snipe-IT category ID.
	CategoryIDs map[string]int `yaml:"category_ids"`
	// DefaultCategoryID is the fallback used when CategoryIDs doesn't map a platform.
	DefaultCategoryID int `yaml:"default_category_id"`
}

// SyncConfig holds sync behavior settings.
type SyncConfig struct {
	DryRun         bool   `yaml:"dry_run"`
	Force          bool   `yaml:"force"`            // ignore timestamps, always update
	RateLimit      bool   `yaml:"rate_limit"`       // enable Snipe-IT rate limiting
	UpdateOnly     bool   `yaml:"update_only"`      // only update existing assets, never create
	UseCache       bool   `yaml:"use_cache"`        // sync from cached hosts.json instead of API
	CacheDir       string `yaml:"cache_dir"`        // default ".cache"
	SetName        bool   `yaml:"set_name"`         // sync hostname into Snipe-IT name field
	AssetTagPrefix string `yaml:"asset_tag_prefix"` // prefix for generated asset tags (default "fleet-")
	// FieldMapping maps a Snipe-IT custom field DB column (e.g. "_snipeit_os_version_3")
	// to a gjson path into the Fleet host JSON (e.g. "os_version", "mdm.enrollment_status",
	// "hardware_serial"). The setup command populates this automatically.
	FieldMapping map[string]string `yaml:"field_mapping"`
	// PlatformFilter optionally restricts the sync to Fleet hosts whose platform
	// matches one of these values (e.g. ["darwin", "windows"]). Empty = all.
	PlatformFilter []string `yaml:"platform_filter"`
	// ModelImages enables fetching device images (appledb.dev for Apple hardware)
	// and attaching them as the Snipe-IT model image at model-create time.
	ModelImages bool `yaml:"model_images"`
	// PolicyMapping maps a Snipe-IT custom field db_column_name to a Fleet
	// policy name. The host's evaluated response ("pass"/"fail"/"") is written
	// into the field. populate_policies is auto-enabled when this is non-empty.
	PolicyMapping map[string]string `yaml:"policy_mapping"`
	// QueryMapping maps a Snipe-IT custom field db_column_name to a saved-query
	// result column. Saved queries must have discard_data=false; each configured
	// query is fetched once per sync run and a per-host lookup table is built.
	QueryMapping map[string]QueryFieldMap `yaml:"query_mapping"`
	// LabelMapping maps a Snipe-IT custom field db_column_name to a Fleet label
	// name. The field is set to "yes" when the host belongs to the label and
	// "no" otherwise. populate_labels is auto-enabled when this is non-empty.
	LabelMapping map[string]string `yaml:"label_mapping"`
	// LabelsField, if set, is a Snipe-IT custom field db_column_name that
	// receives a comma-separated list of every label name the host belongs to.
	// populate_labels is auto-enabled when this is non-empty.
	LabelsField string `yaml:"labels_field"`
}

// QueryFieldMap names a saved Fleet query and the result column to copy
// into a Snipe-IT custom field.
type QueryFieldMap struct {
	Query  string `yaml:"query"`  // saved query name (resolved to ID at warm time)
	Column string `yaml:"column"` // result column from the row Fleet returns
}

// WebhookConfig holds settings for the `serve` subcommand.
type WebhookConfig struct {
	Addr   string `yaml:"addr"`   // listen address, default ":9090"
	Secret string `yaml:"secret"` // shared secret expected on incoming webhooks
	// Path is the URL path Fleet posts to (default "/webhook/fleet").
	Path string `yaml:"path"`
}

// Load reads configuration from a YAML file and applies environment variable
// overrides. Missing file returns an error — callers may swallow it for
// commands that should run without a config (e.g. `--help`).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	// Environment variable overrides
	if v := os.Getenv("FLEET_URL"); v != "" {
		cfg.Fleet.URL = v
	}
	if v := os.Getenv("FLEET_TOKEN"); v != "" {
		cfg.Fleet.Token = v
	}
	if v := os.Getenv("SNIPE_URL"); v != "" {
		cfg.SnipeIT.URL = v
	}
	if v := os.Getenv("SNIPE_API_KEY"); v != "" {
		cfg.SnipeIT.APIKey = v
	}
	if v := os.Getenv("FLEET2SNIPE_WEBHOOK_SECRET"); v != "" {
		cfg.Webhook.Secret = v
	}

	return cfg, nil
}

// Validate checks that required fields are set for a full sync.
// Fleet credentials are not required when UseCache is true.
func (c *Config) Validate() error {
	if !c.Sync.UseCache {
		if err := c.ValidateFleet(); err != nil {
			return err
		}
	}
	return c.ValidateSnipeIT()
}

// ValidateFleet ensures Fleet credentials are set.
func (c *Config) ValidateFleet() error {
	if c.Fleet.URL == "" {
		return fmt.Errorf("fleet.url is required")
	}
	if c.Fleet.Token == "" {
		return fmt.Errorf("fleet.token is required (use an api_only user)")
	}
	return nil
}

// ValidateSnipeIT ensures Snipe-IT credentials and required IDs are set.
func (c *Config) ValidateSnipeIT() error {
	if c.SnipeIT.URL == "" {
		return fmt.Errorf("snipe_it.url is required")
	}
	if c.SnipeIT.APIKey == "" {
		return fmt.Errorf("snipe_it.api_key is required")
	}
	if c.SnipeIT.DefaultStatusID == 0 {
		return fmt.Errorf("snipe_it.default_status_id is required")
	}
	if c.SnipeIT.DefaultCategoryID == 0 && len(c.SnipeIT.CategoryIDs) == 0 {
		return fmt.Errorf("snipe_it.default_category_id or snipe_it.category_ids is required")
	}
	return nil
}

// PerPage returns the configured Fleet page size, defaulting to 1000.
func (c *FleetConfig) EffectivePerPage() int {
	if c.PerPage > 0 {
		return c.PerPage
	}
	return 1000
}

// CategoryIDForPlatform returns the Snipe-IT category ID for a Fleet platform,
// falling back to DefaultCategoryID.
func (c *SnipeITConfig) CategoryIDForPlatform(platform string) int {
	if id, ok := c.CategoryIDs[strings.ToLower(platform)]; ok && id != 0 {
		return id
	}
	return c.DefaultCategoryID
}

// FieldsetIDForPlatform returns the Snipe-IT fieldset ID for a Fleet platform,
// falling back to CustomFieldsetID. Zero is a valid "no fieldset attached".
func (c *SnipeITConfig) FieldsetIDForPlatform(platform string) int {
	if id, ok := c.FieldsetIDs[strings.ToLower(platform)]; ok && id != 0 {
		return id
	}
	return c.CustomFieldsetID
}

// AllFieldsetIDs returns every fieldset id referenced in this config, deduped.
// Used by `setup` to ensure each created custom field is associated with all
// configured fieldsets in one pass.
func (c *SnipeITConfig) AllFieldsetIDs() []int {
	seen := make(map[int]struct{})
	var out []int
	add := func(id int) {
		if id == 0 {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	add(c.CustomFieldsetID)
	for _, id := range c.FieldsetIDs {
		add(id)
	}
	return out
}

// ManufacturerIDForVendor returns the Snipe-IT manufacturer ID for a Fleet
// hardware_vendor, or 0 if not mapped (caller is expected to ensure/create).
func (c *SnipeITConfig) ManufacturerIDForVendor(vendor string) int {
	if vendor == "" {
		return 0
	}
	if id, ok := c.ManufacturerIDs[strings.ToLower(vendor)]; ok {
		return id
	}
	return 0
}

// MergeFieldMapping reads a YAML config file, merges new field mappings into
// sync.field_mapping and writes it back. If replaceValues is non-nil, any
// existing entries whose value is in that set are removed first (used by
// setup to replace stale field IDs with fresh ones). Comments are preserved.
func MergeFieldMapping(path string, newMappings map[string]string, replaceValues map[string]bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing config file: %w", err)
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("unexpected YAML structure")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("expected mapping at root")
	}

	syncNode := findOrCreateMapping(root, "sync")
	fmNode := findOrCreateMapping(syncNode, "field_mapping")

	if len(replaceValues) > 0 {
		kept := fmNode.Content[:0]
		for i := 0; i < len(fmNode.Content)-1; i += 2 {
			if !replaceValues[fmNode.Content[i+1].Value] {
				kept = append(kept, fmNode.Content[i], fmNode.Content[i+1])
			}
		}
		fmNode.Content = kept
	}

	existing := make(map[string]bool)
	for i := 0; i < len(fmNode.Content)-1; i += 2 {
		existing[fmNode.Content[i].Value] = true
	}

	for dbCol, path := range newMappings {
		if dbCol == "" || path == "" || existing[dbCol] {
			continue
		}
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: dbCol, Tag: "!!str"}
		valNode := &yaml.Node{Kind: yaml.ScalarNode, Value: path, Tag: "!!str"}
		fmNode.Content = append(fmNode.Content, keyNode, valNode)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}
	return nil
}

func findOrCreateMapping(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(parent.Content)-1; i += 2 {
		if parent.Content[i].Value == key {
			val := parent.Content[i+1]
			if val.Kind != yaml.MappingNode {
				val.Kind = yaml.MappingNode
				val.Tag = "!!map"
				val.Value = ""
				val.Content = nil
			}
			return val
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}
	valNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, keyNode, valNode)
	return valNode
}
