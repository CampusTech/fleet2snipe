// Package sync implements the core reconciliation between Fleet hosts and
// Snipe-IT assets. The same engine is driven by both the `sync` subcommand
// (full sweep) and the `serve` subcommand (single-host updates from webhooks).
package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	snipeit "github.com/michellepellon/go-snipeit"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/CampusTech/fleet2snipe/config"
	"github.com/CampusTech/fleet2snipe/fleetapi"
	"github.com/CampusTech/fleet2snipe/images"
	"github.com/CampusTech/fleet2snipe/snipe"
)

var log = logrus.New()

// SetLogLevel sets the package logger level.
func SetLogLevel(level logrus.Level) { log.SetLevel(level) }

// SetLogFormatter sets the package logger formatter.
func SetLogFormatter(f logrus.Formatter) { log.SetFormatter(f) }

// SetLogOutput sets the package logger output.
func SetLogOutput(w io.Writer) { log.SetOutput(w) }

// Stats tracks per-run counts.
type Stats struct {
	Total            int
	Created          int
	Updated          int
	Skipped          int
	Errors           int
	ModelsCreated    int
	ManufacturersNew int
}

// Add merges other into s.
func (s *Stats) Add(other Stats) {
	s.Total += other.Total
	s.Created += other.Created
	s.Updated += other.Updated
	s.Skipped += other.Skipped
	s.Errors += other.Errors
	s.ModelsCreated += other.ModelsCreated
	s.ManufacturersNew += other.ManufacturersNew
}

// Engine reconciles Fleet hosts into Snipe-IT assets.
type Engine struct {
	fleet         *fleetapi.Client
	snipe         *snipe.Client
	cfg           *config.Config
	images        *images.Fetcher // nil = image fetching disabled
	models        map[string]int  // hardware_model -> snipe model ID
	manufacturers map[string]int  // hardware_vendor (lowercased) -> snipe manufacturer ID
	stats         Stats

	// queryResults[snipeDBColumn][hostID] = value. Populated during Warm by
	// fetching the report for each saved query in cfg.Sync.QueryMapping once
	// and turning it into a fast per-host lookup. Avoids one API call per host.
	queryResults map[string]map[uint]string
}

// NewEngine constructs an Engine. The Fleet client may be nil if all hosts
// will be supplied directly (e.g. tests or webhook payload-only flows).
func NewEngine(f *fleetapi.Client, s *snipe.Client, cfg *config.Config) *Engine {
	return &Engine{
		fleet:         f,
		snipe:         s,
		cfg:           cfg,
		models:        make(map[string]int),
		manufacturers: make(map[string]int),
		queryResults:  make(map[string]map[uint]string),
	}
}

// Stats returns a copy of the running totals.
func (e *Engine) Stats() Stats { return e.stats }

// WithImages attaches a model-image fetcher. Pass nil to disable (default).
// Returns the engine for chaining.
func (e *Engine) WithImages(f *images.Fetcher) *Engine {
	e.images = f
	return e
}

// Warm loads existing Snipe-IT models and manufacturers into in-memory lookup
// maps. Must be called before SyncHost / SyncAll.
func (e *Engine) Warm(ctx context.Context) error {
	models, err := e.snipe.ListAllModels(ctx)
	if err != nil {
		return fmt.Errorf("loading models: %w", err)
	}
	for _, m := range models {
		if m.ModelNumber != "" {
			e.models[m.ModelNumber] = m.ID
		}
		if m.Name != "" && e.models[m.Name] == 0 {
			e.models[m.Name] = m.ID
		}
	}
	log.WithField("count", len(models)).Info("loaded snipe-it models")

	mfgs, err := e.snipe.ListAllManufacturers(ctx)
	if err != nil {
		return fmt.Errorf("loading manufacturers: %w", err)
	}
	for _, m := range mfgs {
		if m.Name != "" {
			e.manufacturers[strings.ToLower(m.Name)] = m.ID
		}
	}
	for vendor, id := range e.cfg.SnipeIT.ManufacturerIDs {
		e.manufacturers[strings.ToLower(vendor)] = id
	}
	log.WithField("count", len(mfgs)).Info("loaded snipe-it manufacturers")

	if err := e.loadQueryReports(ctx); err != nil {
		// Saved-query mapping failures are non-fatal — degrade by leaving
		// those fields blank rather than aborting the whole sync.
		log.WithError(err).Warn("could not pre-fetch saved-query reports; query_mapping fields will be empty")
	}
	return nil
}

// loadQueryReports resolves each configured query name to an ID, fetches its
// report once, and indexes the result by host_id so SyncHost can do O(1) lookups.
func (e *Engine) loadQueryReports(ctx context.Context) error {
	if len(e.cfg.Sync.QueryMapping) == 0 || e.fleet == nil {
		return nil
	}
	queries, err := e.fleet.ListQueries(ctx)
	if err != nil {
		return fmt.Errorf("listing queries: %w", err)
	}
	idByName := make(map[string]uint, len(queries))
	for _, q := range queries {
		idByName[q.Name] = q.ID
	}
	for dbCol, qm := range e.cfg.Sync.QueryMapping {
		if qm.Query == "" || qm.Column == "" {
			log.WithField("db_column", dbCol).Warn("query_mapping entry missing query or column, skipping")
			continue
		}
		qid, ok := idByName[qm.Query]
		if !ok {
			log.WithFields(logrus.Fields{"db_column": dbCol, "query": qm.Query}).Warn("saved query not found in Fleet, skipping mapping")
			continue
		}
		rows, err := e.fleet.QueryReport(ctx, qid)
		if err != nil {
			log.WithError(err).WithField("query", qm.Query).Warn("could not fetch query report")
			continue
		}
		hostLookup := make(map[uint]string, len(rows))
		for _, r := range rows {
			if v, ok := r.Columns[qm.Column]; ok && v != "" {
				hostLookup[r.HostID] = v
			}
		}
		e.queryResults[dbCol] = hostLookup
		log.WithFields(logrus.Fields{"query": qm.Query, "column": qm.Column, "hosts": len(hostLookup)}).Info("indexed saved-query report")
	}
	return nil
}

// SyncAll iterates a slice of Fleet hosts and reconciles each one.
func (e *Engine) SyncAll(ctx context.Context, hosts []fleetapi.Host) (*Stats, error) {
	for i, h := range hosts {
		if err := ctx.Err(); err != nil {
			return &e.stats, err
		}
		if err := e.SyncHost(ctx, h); err != nil {
			log.WithError(err).WithField("serial", h.HardwareSerial).Error("sync failed")
			e.stats.Errors++
		}
		if (i+1)%50 == 0 {
			log.WithFields(logrus.Fields{"progress": i + 1, "total": len(hosts)}).Info("syncing")
		}
	}
	log.WithFields(logrus.Fields{
		"total":          e.stats.Total,
		"created":        e.stats.Created,
		"updated":        e.stats.Updated,
		"skipped":        e.stats.Skipped,
		"errors":         e.stats.Errors,
		"models_created": e.stats.ModelsCreated,
	}).Info("sync complete")
	return &e.stats, nil
}

// SyncHost reconciles a single Fleet host into Snipe-IT.
func (e *Engine) SyncHost(ctx context.Context, h fleetapi.Host) error {
	e.stats.Total++

	serial := strings.TrimSpace(h.HardwareSerial)
	if serial == "" {
		log.WithField("host_id", h.ID).Debug("skipping host with no serial")
		e.stats.Skipped++
		return nil
	}

	if len(e.cfg.Sync.PlatformFilter) > 0 && !platformMatches(h.Platform, e.cfg.Sync.PlatformFilter) {
		log.WithFields(logrus.Fields{"serial": serial, "platform": h.Platform}).Debug("skipping host (platform filter)")
		e.stats.Skipped++
		return nil
	}

	logger := log.WithField("serial", serial).WithField("host_id", h.ID)

	// Existence check (drives create vs update).
	existing, err := e.snipe.GetAssetBySerial(ctx, serial)
	if err != nil {
		return fmt.Errorf("snipe lookup %s: %w", serial, err)
	}

	switch {
	case existing.Total == 0 && e.cfg.Sync.UpdateOnly:
		logger.Info("asset not in Snipe-IT (update_only) — skipping")
		e.stats.Skipped++
		return nil
	case existing.Total == 0:
		return e.create(ctx, h, logger)
	case existing.Total > 1:
		logger.Warnf("ambiguous: %d Snipe-IT assets share this serial — skipping", existing.Total)
		e.stats.Skipped++
		return nil
	default:
		return e.update(ctx, h, existing.Rows[0], logger)
	}
}

// platformMatches checks whether platform appears (case-insensitively) in allow.
func platformMatches(platform string, allow []string) bool {
	p := strings.ToLower(platform)
	for _, a := range allow {
		if strings.ToLower(a) == p {
			return true
		}
	}
	return false
}

// create inserts a new Snipe-IT asset for the host.
func (e *Engine) create(ctx context.Context, h fleetapi.Host, logger *logrus.Entry) error {
	modelID, err := e.ensureModel(ctx, h, logger)
	if err != nil {
		return fmt.Errorf("ensuring model: %w", err)
	}
	if modelID == 0 {
		logger.Warn("could not resolve model — skipping create")
		e.stats.Skipped++
		return nil
	}

	asset := snipeit.Asset{}
	asset.Serial = h.HardwareSerial
	asset.Model.ID = modelID
	asset.StatusLabel.ID = e.cfg.SnipeIT.DefaultStatusID
	asset.AssetTag = e.assetTag(h)
	if e.cfg.Sync.SetName {
		asset.Name = preferredName(h)
	}
	asset.CustomFields = e.applyMapping(h)

	if e.cfg.Sync.DryRun {
		logger.WithFields(logrus.Fields{
			"model_id":      modelID,
			"asset_tag":     asset.AssetTag,
			"custom_fields": len(asset.CustomFields),
		}).Info("[DRY RUN] would create asset")
		e.stats.Created++
		return nil
	}

	created, err := e.snipe.CreateAsset(ctx, asset)
	if err != nil {
		return err
	}
	logger.WithFields(logrus.Fields{"snipe_id": created.ID, "asset_tag": created.AssetTag}).Info("created asset")
	e.stats.Created++
	return nil
}

// update PATCHes an existing asset with any changed fields.
func (e *Engine) update(ctx context.Context, h fleetapi.Host, existing snipeit.Asset, logger *logrus.Entry) error {
	// Freshness check (Fleet's detail_updated_at vs Snipe's updated_at). Skip
	// when --force is set or when the host has never reported in.
	if !e.cfg.Sync.Force && !h.DetailUpdatedAt.IsZero() && existing.UpdatedAt != nil {
		snipeUpdated := existing.UpdatedAt.Time
		if !snipeUpdated.IsZero() && h.DetailUpdatedAt.Before(snipeUpdated) {
			logger.WithFields(logrus.Fields{
				"fleet_detail_updated_at": h.DetailUpdatedAt,
				"snipe_updated_at":        snipeUpdated,
			}).Debug("snipe is newer — skipping update")
			e.stats.Skipped++
			return nil
		}
	}

	patch := snipeit.Asset{}
	changed := false

	if e.cfg.Sync.SetName {
		if name := preferredName(h); name != "" && name != existing.Name {
			patch.Name = name
			changed = true
		}
	}

	// Ensure the model is correct (devices can swap motherboards / reimaged etc.).
	if modelID, err := e.ensureModel(ctx, h, logger); err == nil && modelID != 0 && modelID != existing.Model.ID {
		patch.Model.ID = modelID
		changed = true
	}

	custom := e.applyMapping(h)
	if len(custom) > 0 {
		diff := diffCustomFields(existing.CustomFields, custom)
		if len(diff) > 0 {
			patch.CustomFields = diff
			changed = true
		}
	}

	if !changed {
		logger.Debug("no changes")
		e.stats.Skipped++
		return nil
	}

	if e.cfg.Sync.DryRun {
		logger.WithFields(logrus.Fields{
			"snipe_id": existing.ID,
			"changes":  custom,
			"set_name": patch.Name,
		}).Info("[DRY RUN] would update asset")
		e.stats.Updated++
		return nil
	}

	if _, err := e.snipe.PatchAsset(ctx, existing.ID, patch); err != nil {
		return err
	}
	logger.WithField("snipe_id", existing.ID).Info("updated asset")
	e.stats.Updated++
	return nil
}

// assetTag returns the asset tag to use when creating a new asset.
// Format: <prefix><fleet host id>. When the prefix is empty the default is "fleet-".
func (e *Engine) assetTag(h fleetapi.Host) string {
	prefix := e.cfg.Sync.AssetTagPrefix
	if prefix == "" {
		prefix = "fleet-"
	}
	return prefix + strconv.FormatUint(uint64(h.ID), 10)
}

// preferredName picks the most useful display string for a host.
func preferredName(h fleetapi.Host) string {
	for _, candidate := range []string{h.DisplayName, h.ComputerName, h.Hostname} {
		if s := strings.TrimSpace(candidate); s != "" {
			return s
		}
	}
	return ""
}

// applyMapping evaluates every configured source against a host and returns
// the merged Snipe-IT custom_field DB column -> value map. Sources:
//   - sync.field_mapping: gjson paths into the host JSON
//   - sync.policy_mapping: pass/fail response from a named Fleet policy
//   - sync.query_mapping:  a column from the host's row in a saved query's report
//   - sync.label_mapping:  "yes"/"no" depending on host membership in a named label
//   - sync.labels_field:   comma-separated list of every label the host belongs to
//
// Empty / missing values are skipped so we never overwrite Snipe data with "".
func (e *Engine) applyMapping(h fleetapi.Host) map[string]string {
	out := make(map[string]string)

	if len(e.cfg.Sync.FieldMapping) > 0 && len(h.Raw) > 0 {
		root := gjson.ParseBytes(h.Raw)
		for dbCol, path := range e.cfg.Sync.FieldMapping {
			if dbCol == "" || path == "" {
				continue
			}
			res := root.Get(path)
			if !res.Exists() {
				continue
			}
			if val := stringifyGJSON(res); val != "" {
				out[dbCol] = val
			}
		}
	}

	for dbCol, policyName := range e.cfg.Sync.PolicyMapping {
		if dbCol == "" || policyName == "" {
			continue
		}
		if v := policyResponse(h.Policies, policyName); v != "" {
			out[dbCol] = v
		}
	}

	for dbCol := range e.cfg.Sync.QueryMapping {
		if v, ok := e.queryResults[dbCol][h.ID]; ok && v != "" {
			out[dbCol] = v
		}
	}

	if len(e.cfg.Sync.LabelMapping) > 0 {
		// Lowercased name set for O(1) per-label membership checks; built once
		// per host rather than O(N*M) nested loops on big label sets.
		set := make(map[string]struct{}, len(h.Labels))
		for _, l := range h.Labels {
			set[strings.ToLower(l.Name)] = struct{}{}
		}
		for dbCol, labelName := range e.cfg.Sync.LabelMapping {
			if dbCol == "" || labelName == "" {
				continue
			}
			if _, ok := set[strings.ToLower(labelName)]; ok {
				out[dbCol] = "yes"
			} else {
				out[dbCol] = "no"
			}
		}
	}

	if e.cfg.Sync.LabelsField != "" && len(h.Labels) > 0 {
		names := make([]string, 0, len(h.Labels))
		for _, l := range h.Labels {
			if n := strings.TrimSpace(l.Name); n != "" {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			sort.Strings(names) // deterministic so we don't churn the field on every sync
			out[e.cfg.Sync.LabelsField] = strings.Join(names, ", ")
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// policyResponse finds a policy by name in the host's policy list and returns
// its response ("pass" / "fail" / "" if never evaluated or not found).
func policyResponse(policies []fleetapi.Policy, name string) string {
	for _, p := range policies {
		if p.Name == name {
			return p.Response
		}
	}
	return ""
}

// stringifyGJSON renders a gjson Result as a single string suitable for
// Snipe-IT custom fields. Arrays are joined with ", ", booleans become true/false,
// timestamps are normalized to RFC3339, numbers to their textual form.
func stringifyGJSON(r gjson.Result) string {
	switch r.Type {
	case gjson.Null:
		return ""
	case gjson.False:
		return "false"
	case gjson.True:
		return "true"
	case gjson.Number:
		// Render integers without a trailing ".0"; let big numbers fall back to raw.
		if r.Num == float64(int64(r.Num)) {
			return strconv.FormatInt(int64(r.Num), 10)
		}
		return strconv.FormatFloat(r.Num, 'f', -1, 64)
	case gjson.String:
		// If the string parses as a time, normalize it.
		if t, err := time.Parse(time.RFC3339, r.Str); err == nil {
			return t.UTC().Format("2006-01-02 15:04:05")
		}
		return r.Str
	case gjson.JSON:
		if r.IsArray() {
			parts := make([]string, 0)
			r.ForEach(func(_, v gjson.Result) bool {
				if s := stringifyGJSON(v); s != "" {
					parts = append(parts, s)
				}
				return true
			})
			return strings.Join(parts, ", ")
		}
		return r.Raw
	}
	return ""
}

// diffCustomFields returns only the keys in newFields whose value differs from
// the existing map. go-snipeit decodes custom fields into a flat
// map[string]string keyed by db_column_name.
func diffCustomFields(existing map[string]string, newFields map[string]string) map[string]string {
	out := make(map[string]string, len(newFields))
	for k, v := range newFields {
		if cur, ok := existing[k]; ok && cur == v {
			continue
		}
		out[k] = v
	}
	return out
}

// ensureModel returns the Snipe-IT model ID for the host's hardware_model,
// creating the model if absent. Uses hardware_model as the model name and
// model number — works well for macOS ("MacBookPro17,1") and is reasonable
// for Windows ("Latitude 5530") and Linux as well.
func (e *Engine) ensureModel(ctx context.Context, h fleetapi.Host, logger *logrus.Entry) (int, error) {
	key := strings.TrimSpace(h.HardwareModel)
	if key == "" {
		key = strings.TrimSpace(h.HardwareVersion)
	}
	if key == "" {
		return 0, nil
	}
	if id, ok := e.models[key]; ok {
		return id, nil
	}

	mfgID, err := e.ensureManufacturer(ctx, h, logger)
	if err != nil {
		return 0, err
	}

	categoryID := e.cfg.SnipeIT.CategoryIDForPlatform(h.Platform)
	if categoryID == 0 {
		return 0, fmt.Errorf("no Snipe-IT category configured for platform %q", h.Platform)
	}

	if e.cfg.Sync.DryRun {
		logger.WithFields(logrus.Fields{
			"model":           key,
			"manufacturer_id": mfgID,
			"category_id":     categoryID,
		}).Info("[DRY RUN] would create model")
		// Return a synthetic non-zero id so the rest of the dry-run can proceed.
		e.models[key] = -1
		e.stats.ModelsCreated++
		return -1, nil
	}

	m := snipeit.Model{}
	m.Name = key
	m.ModelNumber = key
	m.Manufacturer.ID = mfgID
	m.Category.ID = categoryID
	if fsID := e.cfg.SnipeIT.FieldsetIDForPlatform(h.Platform); fsID > 0 {
		m.FieldsetID = fsID
	}
	if e.images != nil {
		if img, err := e.images.ForHost(ctx, h); err != nil {
			logger.WithError(err).Debug("could not fetch model image, continuing without")
		} else if img != "" {
			m.Image = img
		}
	}

	created, err := e.snipe.CreateModel(ctx, m)
	if err != nil {
		return 0, err
	}
	e.models[key] = created.ID
	e.stats.ModelsCreated++
	logger.WithFields(logrus.Fields{"model": key, "snipe_model_id": created.ID}).Info("created snipe-it model")
	return created.ID, nil
}

// ensureManufacturer returns a manufacturer ID for the host's vendor, creating
// one when missing. Returns 0 (and a nil error) when there's no vendor — the
// caller decides whether that's fatal.
func (e *Engine) ensureManufacturer(ctx context.Context, h fleetapi.Host, logger *logrus.Entry) (int, error) {
	vendor := strings.TrimSpace(h.HardwareVendor)
	if vendor == "" {
		// Fall back to platform-based guesses for the common case.
		vendor = vendorFromPlatform(h.Platform)
	}
	if vendor == "" {
		return 0, nil
	}
	if id, ok := e.manufacturers[strings.ToLower(vendor)]; ok && id != 0 {
		return id, nil
	}
	if id := e.cfg.SnipeIT.ManufacturerIDForVendor(vendor); id != 0 {
		e.manufacturers[strings.ToLower(vendor)] = id
		return id, nil
	}

	if e.cfg.Sync.DryRun {
		logger.WithField("manufacturer", vendor).Info("[DRY RUN] would create manufacturer")
		e.manufacturers[strings.ToLower(vendor)] = -1
		e.stats.ManufacturersNew++
		return -1, nil
	}

	created, err := e.snipe.CreateManufacturer(ctx, vendor)
	if err != nil {
		return 0, err
	}
	e.manufacturers[strings.ToLower(vendor)] = created.ID
	e.stats.ManufacturersNew++
	logger.WithFields(logrus.Fields{"manufacturer": vendor, "snipe_id": created.ID}).Info("created snipe-it manufacturer")
	return created.ID, nil
}

// vendorFromPlatform supplies a reasonable manufacturer guess when Fleet's
// hardware_vendor column is empty (common on Linux/Chrome where osquery returns "").
func vendorFromPlatform(platform string) string {
	switch strings.ToLower(platform) {
	case "darwin", "ios", "ipados":
		return "Apple"
	case "chrome":
		return "Google"
	}
	return ""
}

// SerializeHosts marshals hosts to a JSON byte slice for cache files.
func SerializeHosts(hosts []fleetapi.Host) ([]byte, error) {
	return json.MarshalIndent(hosts, "", "  ")
}

// DeserializeHosts loads hosts from a cache file. The Raw field is reconstructed
// so applyMapping continues to work against cache-loaded data.
func DeserializeHosts(data []byte) ([]fleetapi.Host, error) {
	var hosts []fleetapi.Host
	if err := json.Unmarshal(data, &hosts); err != nil {
		return nil, err
	}
	for i := range hosts {
		raw, err := json.Marshal(hosts[i])
		if err != nil {
			return nil, err
		}
		hosts[i].Raw = raw
	}
	return hosts, nil
}
