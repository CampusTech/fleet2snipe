// Package snipe wraps the go-snipeit library with dry-run enforcement, a
// token-bucket rate limiter, and convenience methods used by the fleet2snipe
// sync engine and setup command.
package snipe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	snipeit "github.com/michellepellon/go-snipeit"
	"github.com/sirupsen/logrus"
)

var log = logrus.New()

// SetLogLevel sets the package logger level.
func SetLogLevel(level logrus.Level) { log.SetLevel(level) }

// SetLogFormatter sets the package logger formatter.
func SetLogFormatter(f logrus.Formatter) { log.SetFormatter(f) }

// SetLogOutput sets the package logger output.
func SetLogOutput(w io.Writer) { log.SetOutput(w) }

// ErrDryRun is returned when a write would happen in dry-run mode.
var ErrDryRun = fmt.Errorf("write blocked: dry-run mode is enabled")

// Client wraps go-snipeit with dry-run enforcement.
type Client struct {
	*snipeit.Client
	DryRun bool
}

type snipeLogger struct{}

func (l *snipeLogger) LogRequest(method, url string, body []byte) {
	log.WithFields(logrus.Fields{"method": method, "url": url}).Debug("snipe-it request")
}

func (l *snipeLogger) LogResponse(method, url string, statusCode int, body []byte) {
	log.WithFields(logrus.Fields{"method": method, "url": url, "status": statusCode}).Debug("snipe-it response")
}

// NewClient creates a wrapped go-snipeit client. When rateLimit is true, a
// token-bucket limiter of 2 req/s with burst 5 is applied — same defaults as
// the other CampusTech 2snipe tools.
func NewClient(baseURL, apiKey string, rateLimit bool) (*Client, error) {
	baseURL = strings.TrimRight(baseURL, "/")

	opts := &snipeit.ClientOptions{Logger: &snipeLogger{}}
	if rateLimit {
		opts.RateLimiter = snipeit.NewTokenBucketRateLimiter(2, 5)
	}

	sc, err := snipeit.NewClientWithOptions(baseURL, apiKey, opts)
	if err != nil {
		return nil, fmt.Errorf("creating snipe-it client: %w", err)
	}
	return &Client{Client: sc}, nil
}

// Ping fetches one record to verify the API key works. Used by the test command.
func (c *Client) Ping(ctx context.Context) error {
	_, _, err := c.Assets.ListContext(ctx, &snipeit.ListOptions{Limit: 1})
	return err
}

// ListAllModels pages through every model in Snipe-IT.
func (c *Client) ListAllModels(ctx context.Context) ([]snipeit.Model, error) {
	var all []snipeit.Model
	offset := 0
	const limit = 500
	for {
		resp, _, err := c.Models.ListContext(ctx, &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("listing models: %w", err)
		}
		all = append(all, resp.Rows...)
		if len(all) >= resp.Total {
			break
		}
		offset += limit
	}
	return all, nil
}

// CreateModel creates a new asset model.
func (c *Client) CreateModel(ctx context.Context, m snipeit.Model) (*snipeit.Model, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}
	resp, _, err := c.Models.CreateContext(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("creating model: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("creating model failed: %s", resp.Message)
	}
	return &resp.Payload, nil
}

// PatchModelFieldset attaches a fieldset to an existing model. Uses PATCH so
// only fieldset_id is sent — Snipe-IT re-validates whatever a full update
// carries, which breaks on models with pre-existing validation conflicts.
func (c *Client) PatchModelFieldset(ctx context.Context, modelID, fieldsetID int) error {
	if c.DryRun {
		return ErrDryRun
	}
	m := snipeit.Model{FieldsetID: fieldsetID}
	resp, _, err := c.Models.PatchContext(ctx, modelID, m)
	if err != nil {
		return fmt.Errorf("attaching fieldset %d to model %d: %w", fieldsetID, modelID, err)
	}
	if resp.Status != "success" {
		return fmt.Errorf("attaching fieldset %d to model %d: %s", fieldsetID, modelID, resp.Message)
	}
	return nil
}

// ListAllUsers pages through every Snipe-IT user.
func (c *Client) ListAllUsers(ctx context.Context) ([]snipeit.User, error) {
	var all []snipeit.User
	offset := 0
	const limit = 500
	for {
		resp, _, err := c.Users.ListContext(ctx, &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("listing users: %w", err)
		}
		all = append(all, resp.Rows...)
		if len(all) >= resp.Total {
			break
		}
		offset += limit
	}
	return all, nil
}

// CheckoutAssetToUser checks an asset out to a Snipe-IT user. Errors if the
// asset is already checked out — call CheckinAsset first if reassigning.
func (c *Client) CheckoutAssetToUser(ctx context.Context, assetID, userID int) error {
	if c.DryRun {
		return ErrDryRun
	}
	body := map[string]any{
		"checkout_to_type": "user",
		"assigned_user":    userID,
	}
	resp, _, err := c.Assets.CheckoutContext(ctx, assetID, body)
	if err != nil {
		return fmt.Errorf("checking out asset %d to user %d: %w", assetID, userID, err)
	}
	if resp.Status != "success" {
		return fmt.Errorf("checking out asset %d to user %d: %s", assetID, userID, resp.Message)
	}
	return nil
}

// CheckinAsset returns a checked-out asset back to its base state. Safe to call
// on an asset that isn't currently checked out (Snipe-IT returns a soft error
// which we treat as success).
func (c *Client) CheckinAsset(ctx context.Context, assetID int) error {
	if c.DryRun {
		return ErrDryRun
	}
	resp, _, err := c.Assets.CheckinContext(ctx, assetID, map[string]any{})
	if err != nil {
		return fmt.Errorf("checking in asset %d: %w", assetID, err)
	}
	if resp.Status != "success" {
		// Soft errors meaning "the asset is already unassigned" are fine — we
		// wanted it unassigned and it already is. Snipe-IT has used both
		// "That asset is already checked in." and "...is not checked out..."
		// wordings for this case.
		msg := strings.ToLower(resp.Message.String())
		if strings.Contains(msg, "not checked out") || strings.Contains(msg, "already checked in") {
			return nil
		}
		return fmt.Errorf("checking in asset %d: %s", assetID, resp.Message)
	}
	return nil
}

// ListAllManufacturers pages through every manufacturer.
func (c *Client) ListAllManufacturers(ctx context.Context) ([]snipeit.Manufacturer, error) {
	var all []snipeit.Manufacturer
	offset := 0
	const limit = 500
	for {
		resp, _, err := c.Manufacturers.ListContext(ctx, &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("listing manufacturers: %w", err)
		}
		all = append(all, resp.Rows...)
		if len(all) >= resp.Total {
			break
		}
		offset += limit
	}
	return all, nil
}

// CreateManufacturer creates a manufacturer by name.
func (c *Client) CreateManufacturer(ctx context.Context, name string) (*snipeit.Manufacturer, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}
	m := snipeit.Manufacturer{}
	m.Name = name
	resp, _, err := c.Manufacturers.CreateContext(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("creating manufacturer: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("creating manufacturer failed: %s", resp.Message)
	}
	return &resp.Payload, nil
}

// GetAssetBySerial looks up an asset by serial. Filters to exact case-insensitive
// matches because Snipe's /byserial endpoint performs a partial search.
func (c *Client) GetAssetBySerial(ctx context.Context, serial string) (*snipeit.AssetsResponse, error) {
	resp, _, err := c.Assets.GetAssetBySerialContext(ctx, serial)
	if err != nil {
		return nil, fmt.Errorf("looking up serial %s: %w", serial, err)
	}
	exact := resp.Rows[:0]
	for _, a := range resp.Rows {
		if strings.EqualFold(a.Serial, serial) {
			exact = append(exact, a)
		}
	}
	resp.Rows = exact
	resp.Total = len(exact)
	return resp, nil
}

// CreateAsset creates a hardware asset.
func (c *Client) CreateAsset(ctx context.Context, a snipeit.Asset) (*snipeit.Asset, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}
	resp, _, err := c.Assets.CreateContext(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("creating asset: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("creating asset failed: %s", resp.Message)
	}
	return &resp.Payload, nil
}

// PatchAsset partially updates an asset. On custom-field validation errors,
// strips the rejected fields and retries once so the rest of the update applies.
func (c *Client) PatchAsset(ctx context.Context, id int, a snipeit.Asset) (*snipeit.Asset, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}
	resp, _, err := c.Assets.PatchContext(ctx, id, a)
	if err != nil {
		return nil, fmt.Errorf("updating asset %d: %w", id, err)
	}
	if resp.Status == "success" {
		return &resp.Payload, nil
	}

	rejected, reason := invalidFieldErrors(string(resp.Message))
	if len(rejected) > 0 && a.CustomFields != nil {
		warnFields := logrus.Fields{
			"asset_id": id,
			"fields":   rejected,
			"reason":   reason,
		}
		// The patch itself rarely carries the model, so fetch the asset to
		// name the model whose fieldset needs fixing.
		if got, _, err := c.Assets.GetContext(ctx, id); err == nil {
			warnFields["model_id"] = got.Model.ID
			warnFields["model_name"] = got.Model.Name
		}
		log.WithFields(warnFields).Warn("Snipe-IT rejected custom fields — retrying without them. Run 'fleet2snipe setup' to fix the fieldset.")
		cleaned := make(map[string]string, len(a.CustomFields))
		for k, v := range a.CustomFields {
			cleaned[k] = v
		}
		for _, k := range rejected {
			delete(cleaned, k)
		}
		a.CustomFields = cleaned
		resp, _, err = c.Assets.PatchContext(ctx, id, a)
		if err != nil {
			return nil, fmt.Errorf("updating asset %d (retry): %w", id, err)
		}
		if resp.Status != "success" {
			return nil, fmt.Errorf("updating asset %d failed: %s", id, resp.Message)
		}
		return &resp.Payload, nil
	}
	return nil, fmt.Errorf("updating asset %d failed: %s", id, resp.Message)
}

// invalidFieldErrors parses a Snipe-IT validation error message and returns
// the custom-field keys that should be stripped on retry.
func invalidFieldErrors(msg string) ([]string, string) {
	var errs map[string][]string
	if err := json.Unmarshal([]byte(msg), &errs); err != nil {
		return nil, ""
	}
	var rejected []string
	reason := ""
fieldLoop:
	for key, msgs := range errs {
		for _, m := range msgs {
			switch {
			case strings.Contains(m, "not available on this Asset Model's fieldset"):
				rejected = append(rejected, key)
				reason = "fieldset missing"
				continue fieldLoop
			case strings.Contains(m, "is invalid."):
				rejected = append(rejected, key)
				reason = "invalid field value"
				continue fieldLoop
			}
		}
	}
	return rejected, reason
}

// FieldDef defines a custom field to ensure exists.
type FieldDef struct {
	Name        string
	Element     string // text, textarea, radio, listbox, checkbox
	Format      string // ANY, DATE, BOOLEAN, MAC, IP, NUMERIC, EMAIL, URL
	HelpText    string
	FieldValues string // newline-separated for radio/listbox
}

// SetupFields creates/updates the listed custom fields and associates each one
// with every fieldset in fieldsetIDs. Returns a map of field name ->
// db_column_name. Snipe-IT fields have a single global db_column_name no matter
// how many fieldsets reference them, so multi-fieldset support is purely an
// additional Associate call per fieldset.
func (c *Client) SetupFields(fieldsetIDs []int, fields []FieldDef) (map[string]string, error) {
	if c.DryRun {
		return nil, ErrDryRun
	}
	existing, _, err := c.Fields.List(nil)
	if err != nil {
		return nil, fmt.Errorf("listing existing fields: %w", err)
	}
	byName := make(map[string]snipeit.Field, len(existing.Rows))
	for _, f := range existing.Rows {
		byName[f.Name] = f
	}

	out := make(map[string]string, len(fields))
	for _, f := range fields {
		field := snipeit.Field{}
		field.Name = f.Name
		field.Element = f.Element
		field.Format = f.Format
		field.HelpText = f.HelpText
		field.FieldValues = f.FieldValues

		var fieldID int
		var dbColumn string

		if ex, ok := byName[f.Name]; ok {
			resp, _, err := c.Fields.Update(ex.ID, field)
			if err != nil {
				return out, fmt.Errorf("updating field %q: %w", f.Name, err)
			}
			if resp.Status != "success" {
				return out, fmt.Errorf("updating field %q: %s", f.Name, resp.Message)
			}
			fieldID = resp.Payload.ID
			dbColumn = resp.Payload.DBColumnName
			if dbColumn == "" {
				dbColumn = ex.DBColumnName
			}
		} else {
			resp, _, err := c.Fields.Create(field)
			if err != nil {
				return out, fmt.Errorf("creating field %q: %w", f.Name, err)
			}
			if resp.Status != "success" {
				return out, fmt.Errorf("creating field %q: %s", f.Name, resp.Message)
			}
			fieldID = resp.Payload.ID
			dbColumn = resp.Payload.DBColumnName
		}

		out[f.Name] = dbColumn

		for _, fsID := range fieldsetIDs {
			if fsID <= 0 {
				continue
			}
			if _, err := c.Fields.Associate(fieldID, fsID); err != nil {
				return out, fmt.Errorf("associating %q with fieldset %d: %w", f.Name, fsID, err)
			}
		}
	}

	// Snipe-IT sometimes returns blank db_column_name on update — refetch.
	missing := false
	for _, v := range out {
		if v == "" {
			missing = true
			break
		}
	}
	if missing {
		if refresh, _, err := c.Fields.List(nil); err == nil {
			lut := make(map[string]string, len(refresh.Rows))
			for _, f := range refresh.Rows {
				lut[f.Name] = f.DBColumnName
			}
			for name, dbCol := range out {
				if dbCol == "" {
					if col, ok := lut[name]; ok && col != "" {
						out[name] = col
					}
				}
			}
		}
	}

	return out, nil
}
