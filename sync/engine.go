// Package sync implements the core reconciliation between Fleet hosts and
// Snipe-IT assets. The same engine is driven by both the `sync` subcommand
// (full sweep) and the `serve` subcommand (single-host updates from webhooks).
package sync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
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
	CheckoutsApplied int
	CheckoutsSkipped int
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
	s.CheckoutsApplied += other.CheckoutsApplied
	s.CheckoutsSkipped += other.CheckoutsSkipped
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

	// queryIDByName resolves saved query names to their Fleet IDs. Populated
	// during Warm from the union of global + per-platform query mappings.
	queryIDByName map[string]uint
	// queryRows[queryID][hostID] = the result row's columns for that host.
	// Pre-fetched once per unique saved query during Warm so per-host lookups
	// stay O(1) regardless of platform or fleet size.
	queryRows map[uint]map[uint]map[string]string

	// usersByKey indexes Snipe-IT users by the configured MatchField, lowercased.
	// Populated during Warm when cfg.Sync.Checkout.Enabled. Nil otherwise.
	usersByKey map[string]int
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
		queryIDByName: make(map[string]uint),
		queryRows:     make(map[uint]map[uint]map[string]string),
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

	if e.cfg.Sync.Checkout.Enabled {
		if err := e.loadUsers(ctx); err != nil {
			log.WithError(err).Warn("could not load Snipe-IT users; checkout will be skipped for this run")
		}
	}
	return nil
}

// loadUsers pages every Snipe-IT user and indexes them by the configured
// match field. The lowercased key makes lookups case-insensitive (Fleet's
// usernames and Snipe-IT's often disagree on casing for the same person).
func (e *Engine) loadUsers(ctx context.Context) error {
	users, err := e.snipe.ListAllUsers(ctx)
	if err != nil {
		return fmt.Errorf("listing users: %w", err)
	}
	matchField := strings.ToLower(strings.TrimSpace(e.cfg.Sync.Checkout.MatchField))
	if matchField == "" {
		matchField = "username"
	}
	idx := make(map[string]int, len(users))
	for _, u := range users {
		if !u.Activated {
			continue
		}
		var key string
		switch matchField {
		case "email":
			key = u.Email
		case "employee_num":
			key = u.Employee
		case "username":
			key = u.Username
		default:
			return fmt.Errorf("invalid checkout.match_field %q (expected username, email, or employee_num)", matchField)
		}
		if key == "" {
			continue
		}
		idx[strings.ToLower(strings.TrimSpace(key))] = u.ID
	}
	e.usersByKey = idx
	log.WithFields(logrus.Fields{"count": len(idx), "match_field": matchField}).Info("indexed snipe-it users for checkout")
	return nil
}

// loadQueryReports resolves every saved-query name referenced across global
// and per-platform query_mappings, fetches each report exactly once, and
// indexes the result by host_id. Per-host application reads queryRows
// directly so a single global query referenced by N platforms still costs
// one Fleet API call.
func (e *Engine) loadQueryReports(ctx context.Context) error {
	names := e.cfg.Sync.AllQueryNames()
	if len(names) == 0 || e.fleet == nil {
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
	for _, name := range names {
		qid, ok := idByName[name]
		if !ok {
			log.WithField("query", name).Warn("saved query not found in Fleet, skipping")
			continue
		}
		e.queryIDByName[name] = qid
		rows, err := e.fleet.QueryReport(ctx, qid)
		if err != nil {
			log.WithError(err).WithField("query", name).Warn("could not fetch query report")
			continue
		}
		byHost := make(map[uint]map[string]string, len(rows))
		for _, r := range rows {
			byHost[r.HostID] = r.Columns
		}
		e.queryRows[qid] = byHost
		log.WithFields(logrus.Fields{"query": name, "hosts": len(byHost)}).Info("indexed saved-query report")
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
	// A freshly created asset has no assignment, so we don't need the current
	// user — pass 0 for that arg.
	e.applyCheckout(ctx, h, *created, 0, logger)
	return nil
}

// update PATCHes an existing asset with any changed fields.
func (e *Engine) update(ctx context.Context, h fleetapi.Host, existing snipeit.Asset, logger *logrus.Entry) error {
	// Freshness check (Fleet's detail_updated_at vs Snipe's updated_at). Skip
	// when --force is set or when the host's details were never fetched (Fleet
	// reports its NeverTimestamp sentinel then, not a zero time).
	if !e.cfg.Sync.Force && h.DetailsFetched() && existing.UpdatedAt != nil {
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

	currentUserID := 0
	if existing.User != nil {
		currentUserID = existing.User.ID
	}

	if !changed {
		logger.Debug("no field changes")
		e.stats.Skipped++
		// Even when no field changes, the user assignment can still need work
		// (someone left the company, MDM reassigned the device, etc.).
		e.applyCheckout(ctx, h, existing, currentUserID, logger)
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
	e.applyCheckout(ctx, h, existing, currentUserID, logger)
	return nil
}

// applyCheckout looks at the configured user field on the Fleet host, finds
// the matching Snipe-IT user, and adjusts the asset's assignment per mode.
// All failures are logged and absorbed — never block sync progress on checkout.
func (e *Engine) applyCheckout(ctx context.Context, h fleetapi.Host, asset snipeit.Asset, currentUserID int, logger *logrus.Entry) {
	if !e.cfg.Sync.Checkout.Enabled || e.usersByKey == nil {
		return
	}
	if e.cfg.Sync.Checkout.UserField == "" {
		return
	}
	if asset.ID == 0 {
		// Dry-run or create that didn't return an id — nothing to act on.
		return
	}

	rawKey := extractCheckoutKey(h.Raw, e.cfg.Sync.Checkout.UserField)
	if rawKey == "" && e.fleet != nil && h.ID != 0 {
		// The list endpoint omits detail-only fields like end_users (IdP
		// mapping), so a miss against list-shaped Raw isn't conclusive —
		// retry against the full host detail.
		logger.WithField("user_field", e.cfg.Sync.Checkout.UserField).Debug("user field missing from host JSON; fetching host detail")
		detail, err := e.fleet.GetHost(ctx, h.ID)
		if err != nil {
			logger.WithError(err).Warn("could not fetch host detail for checkout; leaving checkout untouched")
			e.stats.CheckoutsSkipped++
			return
		}
		rawKey = extractCheckoutKey(detail.Raw, e.cfg.Sync.Checkout.UserField)
	}
	if rawKey == "" {
		logger.Debug("no user identifier on host; leaving checkout untouched")
		e.stats.CheckoutsSkipped++
		return
	}

	desiredID, ok := e.usersByKey[strings.ToLower(strings.TrimSpace(rawKey))]
	if !ok {
		logger.WithField("user_key", rawKey).Info("Fleet user not found in Snipe-IT; skipping checkout")
		e.stats.CheckoutsSkipped++
		return
	}

	mode := strings.ToLower(strings.TrimSpace(e.cfg.Sync.Checkout.Mode))
	if mode == "" {
		mode = "assign"
	}

	switch {
	case currentUserID == desiredID && mode != "force":
		// Already correct. Nothing to do.
		return
	case currentUserID != 0 && mode == "assign":
		logger.WithFields(logrus.Fields{
			"current_user_id": currentUserID,
			"desired_user_id": desiredID,
			"mode":            mode,
		}).Debug("asset already assigned; mode=assign so leaving alone")
		e.stats.CheckoutsSkipped++
		return
	}

	// Snipe-IT refuses to check out assets whose status label isn't
	// deployable ("That asset is not available for checkout!"). Skip with a
	// clear message instead — typically an asset marked Lost/Archived whose
	// device is nonetheless still reporting to Fleet.
	if !statusDeployable(asset.StatusLabel) {
		logger.WithFields(logrus.Fields{
			"snipe_id":    asset.ID,
			"status":      asset.StatusLabel.Name,
			"status_type": asset.StatusLabel.StatusType,
			"user_key":    rawKey,
		}).Warn("asset status is not deployable; skipping checkout — host still reports to Fleet, so the Snipe-IT status may be stale")
		e.stats.CheckoutsSkipped++
		return
	}

	if e.cfg.Sync.DryRun {
		logger.WithFields(logrus.Fields{"snipe_id": asset.ID, "user_id": desiredID, "user_key": rawKey}).Info("[DRY RUN] would check out asset")
		e.stats.CheckoutsApplied++
		return
	}

	// Snipe-IT's checkout endpoint refuses to overwrite an existing assignment.
	// Check the asset in first when reassigning.
	if currentUserID != 0 && currentUserID != desiredID {
		if err := e.snipe.CheckinAsset(ctx, asset.ID); err != nil {
			logger.WithError(err).Warn("could not check in asset before reassign")
			e.stats.Errors++
			return
		}
	}

	if err := e.snipe.CheckoutAssetToUser(ctx, asset.ID, desiredID); err != nil {
		logger.WithError(err).WithFields(logrus.Fields{"snipe_id": asset.ID, "user_id": desiredID}).Warn("checkout failed")
		e.stats.Errors++
		return
	}
	logger.WithFields(logrus.Fields{"snipe_id": asset.ID, "user_id": desiredID, "user_key": rawKey}).Info("checked out asset to user")
	e.stats.CheckoutsApplied++
}

// statusDeployable reports whether a status label allows checkout. Archived,
// pending, and undeployable labels are rejected by Snipe-IT's checkout
// endpoint. "deployed" (currently assigned) still passes: reassignment checks
// the asset in first. Unknown/empty types pass so we don't skip on partial
// API responses — worst case Snipe-IT rejects the checkout as before.
func statusDeployable(s snipeit.StatusLabel) bool {
	for _, v := range []string{s.StatusType, s.StatusMeta} {
		switch strings.ToLower(v) {
		case "archived", "pending", "undeployable":
			return false
		}
	}
	return true
}

// extractCheckoutKey evaluates a gjson path against the host's raw JSON.
// Returns the trimmed string value, or "" if the path is missing/empty.
func extractCheckoutKey(raw []byte, path string) string {
	if len(raw) == 0 || path == "" {
		return ""
	}
	res := gjson.GetBytes(raw, path)
	if !res.Exists() {
		return ""
	}
	return strings.TrimSpace(res.String())
}

// assetTag returns the asset tag to use when creating a new asset.
// Resolution order:
//  1. sync.asset_tag.platform_templates[host.platform] — per-platform override
//  2. sync.asset_tag.template — global template
//  3. legacy: sync.asset_tag_prefix + "{id}"
//  4. default: "fleet-{id}"
//
// An empty resolved template means "let Snipe-IT auto-assign" — the engine
// passes "" to the Asset, and go-snipeit's MarshalJSON omits the field.
func (e *Engine) assetTag(h fleetapi.Host) string {
	tpl, explicit := e.effectiveAssetTagTemplate(h.Platform)
	if explicit && tpl == "" {
		// User deliberately set an empty template — auto-assign.
		return ""
	}
	if tpl == "" {
		// Nothing configured; use the historical default.
		tpl = "fleet-{id}"
		if e.cfg.Sync.AssetTagPrefix != "" {
			tpl = e.cfg.Sync.AssetTagPrefix + "{id}"
		}
	}
	return renderAssetTag(tpl, h)
}

// effectiveAssetTagTemplate picks the right template for a platform, plus a
// boolean indicating whether the user explicitly configured one (even if "").
// This distinguishes "auto-assign please" from "use the legacy/default".
func (e *Engine) effectiveAssetTagTemplate(platform string) (string, bool) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if v, ok := e.cfg.Sync.AssetTag.PlatformTemplates[platform]; ok {
		return v, true
	}
	// Look up by lowercased key explicitly — YAML preserves the user's casing.
	for k, v := range e.cfg.Sync.AssetTag.PlatformTemplates {
		if strings.ToLower(k) == platform {
			return v, true
		}
	}
	// Distinguish "template field present" from "absent" by checking the parent
	// struct's zero state — but Go can't, so look at whether either side of the
	// asset_tag block is non-empty. Both empty + no key = "not configured".
	if e.cfg.Sync.AssetTag.Template != "" {
		return e.cfg.Sync.AssetTag.Template, true
	}
	return "", false
}

// renderAssetTag interpolates {gjson.path} placeholders in a template against
// the host's raw JSON. Unmatched placeholders are replaced with "".
func renderAssetTag(template string, h fleetapi.Host) string {
	if template == "" {
		return ""
	}
	raw := h.Raw
	return assetTagPlaceholderRe.ReplaceAllStringFunc(template, func(m string) string {
		path := strings.TrimSpace(m[1 : len(m)-1])
		if path == "" {
			return ""
		}
		if len(raw) == 0 {
			// Fall back to common fields directly off the struct when Raw is empty
			// (e.g. unit tests that don't round-trip through JSON).
			switch path {
			case "id":
				return strconv.FormatUint(uint64(h.ID), 10)
			case "hardware_serial":
				return h.HardwareSerial
			case "hardware_model":
				return h.HardwareModel
			case "hostname":
				return h.Hostname
			case "platform":
				return h.Platform
			case "uuid":
				return h.UUID
			}
			return ""
		}
		return gjson.GetBytes(raw, path).String()
	})
}

// assetTagPlaceholderRe matches {anything-but-braces} placeholders. The path
// inside the braces is treated as a gjson path.
var assetTagPlaceholderRe = regexp.MustCompile(`\{[^{}]+\}`)

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
// the merged Snipe-IT custom_field DB column -> value map. Each mapping type
// pulls the union of its global config plus any per-platform overrides for
// the host's platform, so iOS hosts only see iOS-relevant mappings, etc.
//
// Sources:
//   - sync.field_mapping(+per_platform): gjson paths into the host JSON
//   - sync.policy_mapping(+per_platform): pass/fail response from a named Fleet policy
//   - sync.query_mapping(+per_platform):  a column from the host's row in a saved query's report
//   - sync.label_mapping(+per_platform):  "yes"/"no" depending on host membership in a named label
//   - sync.labels_field:                  comma-separated list of every label the host belongs to
//
// Empty / missing values are skipped so we never overwrite Snipe data with "".
func (e *Engine) applyMapping(h fleetapi.Host) map[string]string {
	out := make(map[string]string)

	fieldMap := e.cfg.Sync.MergedFieldMapping(h.Platform)
	if len(fieldMap) > 0 && len(h.Raw) > 0 {
		root := gjson.ParseBytes(h.Raw)
		for dbCol, entry := range fieldMap {
			if dbCol == "" || entry.Path == "" {
				continue
			}
			res := root.Get(entry.Path)
			if !res.Exists() {
				continue
			}
			if val := transformValue(res, entry.Transform); val != "" {
				out[dbCol] = val
			}
		}
	}

	for dbCol, policyName := range e.cfg.Sync.MergedPolicyMapping(h.Platform) {
		if dbCol == "" || policyName == "" {
			continue
		}
		if v := policyResponse(h.Policies, policyName); v != "" {
			out[dbCol] = v
		}
	}

	for dbCol, qm := range e.cfg.Sync.MergedQueryMapping(h.Platform) {
		if qm.Query == "" || qm.Column == "" {
			continue
		}
		qid, ok := e.queryIDByName[qm.Query]
		if !ok {
			continue
		}
		cols, ok := e.queryRows[qid][h.ID]
		if !ok {
			continue
		}
		if v, ok := cols[qm.Column]; ok && v != "" {
			if qm.Transform != "" {
				v = transformString(v, qm.Transform)
			}
			if v != "" {
				out[dbCol] = v
			}
		}
	}

	if labelMap := e.cfg.Sync.MergedLabelMapping(h.Platform); len(labelMap) > 0 {
		// Lowercased name set for O(1) per-label membership checks; built once
		// per host rather than O(N*M) nested loops on big label sets.
		set := make(map[string]struct{}, len(h.Labels))
		for _, l := range h.Labels {
			set[strings.ToLower(l.Name)] = struct{}{}
		}
		for dbCol, labelName := range labelMap {
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

// transformValue post-processes a resolved gjson value for Snipe-IT storage.
// transform == "" means write the raw value via stringifyGJSON. The other
// recognised transforms either standardise units (bytes/GiB → GB/MB/TB,
// unix → ISO timestamp) or apply lightweight string normalisation (case,
// MAC separators, boolean rendering, thousands grouping).
//
// Numeric "no data" inputs (0, missing, unparseable) all return "" for the
// unit conversions — we never overwrite real Snipe-IT data with a placeholder
// from a host that hasn't reported. For purely cosmetic transforms
// (comma_thousands, bool_yes_no), legitimate zero values pass through.
//
// Transform names are validated at config load time; an unknown value here
// would only fire if validation was bypassed, in which case we degrade
// gracefully to the raw string instead of dropping the data.
func transformValue(r gjson.Result, transform string) string {
	switch transform {
	case "":
		return stringifyGJSON(r)

	// --- Byte-scale conversions (input: int64 bytes) ---

	case "bytes_to_gb":
		// Fleet's memory field is int64 bytes. Decimal GB = bytes / 1_000_000_000.
		n := r.Int()
		if n == 0 {
			return ""
		}
		return strconv.FormatInt(int64(math.Round(float64(n)/1e9)), 10)
	case "bytes_to_gib":
		// Binary GiB = bytes / 2^30. Apple/macOS-style: "48GB" really means 48 GiB.
		n := r.Int()
		if n == 0 {
			return ""
		}
		return strconv.FormatInt(int64(math.Round(float64(n)/1073741824.0)), 10)
	case "bytes_to_mb":
		n := r.Int()
		if n == 0 {
			return ""
		}
		return strconv.FormatInt(int64(math.Round(float64(n)/1e6)), 10)
	case "bytes_to_tb":
		n := r.Int()
		if n == 0 {
			return ""
		}
		return strconv.FormatInt(int64(math.Round(float64(n)/1e12)), 10)
	case "gib_to_gb":
		// Fleet's disk-space fields (gigs_total_disk_space, gigs_disk_space_available)
		// are float GiB. GB = GiB * (2^30 / 10^9) = GiB * 1.073741824.
		f := r.Float()
		if f == 0 {
			return ""
		}
		return strconv.FormatInt(int64(math.Round(f*(1073741824.0/1000000000.0))), 10)

	// --- Time ---

	case "unix_to_iso":
		// Render Unix epoch seconds as Snipe's display format in UTC. Matches
		// what stringifyGJSON does with RFC3339 input so all timestamps look
		// the same across fields. A zero timestamp (1970-01-01) is treated as
		// "missing data" rather than a real value.
		n := r.Int()
		if n == 0 {
			return ""
		}
		return time.Unix(n, 0).UTC().Format("2006-01-02 15:04:05")

	// --- String case ---

	case "uppercase":
		s := r.String()
		if s == "" {
			return ""
		}
		return strings.ToUpper(s)
	case "lowercase":
		s := r.String()
		if s == "" {
			return ""
		}
		return strings.ToLower(s)

	// --- MAC normalisation ---

	case "mac_colons":
		return normalizeMAC(r.String(), ":")
	case "mac_dashes":
		return normalizeMAC(r.String(), "-")
	case "base64_to_mac":
		// Decode 6 base64 bytes into a lowercase colon-separated MAC. Used for
		// Fleet's ioreg table on macOS, which surfaces IOMACAddress as a plist
		// <data> block — e.g. "cIzyxNK1" → "70:8c:f2:c4:d2:b5". The trimmed
		// gjson value is base64-decoded; anything that doesn't yield exactly
		// 6 bytes returns "" so partial / corrupt rows can't write a half MAC.
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.String()))
		if err != nil || len(raw) != 6 {
			return ""
		}
		return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
			raw[0], raw[1], raw[2], raw[3], raw[4], raw[5])

	// --- Display helpers ---

	case "comma_thousands":
		// Whole numbers get comma grouping; floats fall back to default format
		// since "1,234.56" thousands grouping on a decimal isn't well-defined
		// for our use case (Snipe-IT custom fields are display-only here).
		switch r.Type {
		case gjson.Number:
			if r.Num == math.Trunc(r.Num) {
				return commaThousands(int64(r.Num))
			}
			return strconv.FormatFloat(r.Num, 'f', -1, 64)
		case gjson.String:
			if n, err := strconv.ParseInt(strings.TrimSpace(r.Str), 10, 64); err == nil {
				return commaThousands(n)
			}
		}
		return ""

	case "bool_yes_no":
		switch r.Type {
		case gjson.True:
			return "Yes"
		case gjson.False:
			return "No"
		case gjson.Number:
			if r.Num == 0 {
				return "No"
			}
			return "Yes"
		case gjson.String:
			switch strings.ToLower(strings.TrimSpace(r.Str)) {
			case "true", "yes", "1", "y", "t":
				return "Yes"
			case "false", "no", "0", "n", "f":
				return "No"
			}
		}
		return ""

	default:
		// Shouldn't be reachable — config validation rejects unknown transforms
		// before we get here — but fall back to raw rather than dropping data.
		return stringifyGJSON(r)
	}
}

// transformString runs a transform against a bare string value (e.g. a saved
// query column, which always arrives as a string regardless of the underlying
// osquery column type). For numeric transforms (bytes_to_*, gib_to_gb,
// unix_to_iso, comma_thousands) we reparse the string as a JSON number first
// so r.Int() / r.Float() can pick it up; non-numeric strings fall through to
// the string-typed transforms.
func transformString(s, transform string) string {
	if transform == "" {
		return s
	}
	var raw []byte
	if _, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
		raw = []byte(strings.TrimSpace(s))
	} else if _, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		raw = []byte(strings.TrimSpace(s))
	} else {
		// Encode as JSON string so gjson sees Type=String.
		encoded, err := json.Marshal(s)
		if err != nil {
			return s
		}
		raw = encoded
	}
	return transformValue(gjson.ParseBytes(raw), transform)
}

// normalizeMAC strips every non-hex character from s, then re-inserts sep
// between byte pairs and lowercases the hex. Returns "" when the input
// doesn't contain exactly 12 hex characters — handles colon, dash, dot
// (Cisco), and no-separator forms uniformly.
func normalizeMAC(s, sep string) string {
	var hex strings.Builder
	hex.Grow(12)
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
			hex.WriteRune(r)
		}
	}
	h := strings.ToLower(hex.String())
	if len(h) != 12 {
		return ""
	}
	var out strings.Builder
	out.Grow(17)
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			out.WriteString(sep)
		}
		out.WriteString(h[i : i+2])
	}
	return out.String()
}

// commaThousands formats an int64 with US-style thousands separators.
// 1234567 → "1,234,567". No localization; Snipe-IT custom fields are display
// strings so we can keep it simple.
func commaThousands(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	var out strings.Builder
	out.Grow(len(s) + len(s)/3)
	if neg {
		out.WriteByte('-')
	}
	out.WriteString(s[:first])
	for i := first; i < len(s); i += 3 {
		out.WriteByte(',')
		out.WriteString(s[i : i+3])
	}
	return out.String()
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
