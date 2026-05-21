package cmd

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/CampusTech/fleet2snipe/fleetapi"
	f2sync "github.com/CampusTech/fleet2snipe/sync"
)

// NewServeCmd returns the `serve` subcommand — a long-running HTTP listener
// that accepts Fleet's activities webhook and reconciles the affected host
// into Snipe-IT in near-real-time. This is the only Fleet webhook that emits
// inventory-relevant events (enrollment, team transfer, MDM state, deletion).
func NewServeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "serve",
		Short: "Listen for Fleet activities webhooks and update Snipe-IT",
		Long: `Starts an HTTP server that receives Fleet's activities webhook (Settings → Integrations → Automations → Activities) and reconciles inventory-relevant events into Snipe-IT.

Inventory-relevant activity types handled:
  enrolled_host          — new host showed up in Fleet → create/update in Snipe-IT
  refetched_host         — manual refetch → re-sync fresh detail
  mdm_enrolled           — MDM enrollment state changed → re-sync
  mdm_unenrolled         — MDM unenrollment → re-sync
  transferred_hosts      — host moved between teams → re-sync (multiple hosts)
  deleted_host           — host removed from Fleet → logged (Snipe-IT asset is left in place)
  deleted_multiple_hosts — bulk delete → logged

All other activity types are accepted and silently ignored.

For "every host updated" semantics (detail_updated_at changes), run the sync
subcommand on a cron — Fleet does not emit per-update webhooks.

Configure Fleet to POST to <addr><path> (default ":9090/webhook/fleet"). Auth:
the configured shared secret is accepted in the X-Fleet2Snipe-Secret header or
the ?secret= query string. GET <path>/healthz returns 200 OK.`,
		RunE: runServe,
	}
	c.Flags().String("addr", "", "Listen address (overrides webhook.addr in config)")
	c.Flags().String("path", "", "URL path Fleet posts to (overrides webhook.path in config)")
	return c
}

func runServe(cmd *cobra.Command, _ []string) error {
	if err := Cfg.Validate(); err != nil {
		return err
	}

	addr := Cfg.Webhook.Addr
	if v, _ := cmd.Flags().GetString("addr"); v != "" {
		addr = v
	}
	if addr == "" {
		addr = ":9090"
	}
	path := Cfg.Webhook.Path
	if v, _ := cmd.Flags().GetString("path"); v != "" {
		path = v
	}
	if path == "" {
		path = "/webhook/fleet"
	}

	if Cfg.Webhook.Secret == "" {
		log.Warn("webhook.secret is empty — anyone able to reach this endpoint can trigger syncs. Set webhook.secret in settings.yaml or via FLEET2SNIPE_WEBHOOK_SECRET.")
	}

	ctx, cancel := contextWithSignal()
	defer cancel()

	fleetClient, err := newFleetClient()
	if err != nil {
		return err
	}
	snipeClient, err := newSnipeClient()
	if err != nil {
		return err
	}

	engine := f2sync.NewEngine(fleetClient, snipeClient, Cfg)
	if err := engine.Warm(ctx); err != nil {
		return err
	}

	mux := http.NewServeMux()
	h := &activitiesHandler{
		engine: engine,
		fleet:  fleetClient,
		secret: Cfg.Webhook.Secret,
	}
	mux.Handle(path, h)
	mux.HandleFunc(strings.TrimRight(path, "/")+"/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           accessLog(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Infof("fleet2snipe activities listener on %s%s", addr, path)
	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// activitiesHandler dispatches Fleet activities to single-host syncs.
type activitiesHandler struct {
	engine *f2sync.Engine
	fleet  *fleetapi.Client
	secret string
}

// activity is one entry from the Fleet activity feed. Only the fields we
// dispatch on are decoded explicitly; the rest live in Details for type-specific
// lookup. Fleet's wire format for the activities webhook posts an envelope with
// {"activities": [...]} but we also accept a bare array or a single object.
type activity struct {
	ID            uint64          `json:"id"`
	CreatedAt     string          `json:"created_at"`
	ActorFullName string          `json:"actor_full_name"`
	Type          string          `json:"type"`
	Details       json.RawMessage `json:"details"`
}

// Common detail shapes — Fleet uses different field names per activity type.
type singleHostDetails struct {
	HostID          uint   `json:"host_id"`
	HostSerial      string `json:"host_serial"`
	HostDisplayName string `json:"host_display_name"`
}

type multiHostDetails struct {
	HostIDs   []uint   `json:"host_ids"`
	HostNames []string `json:"host_display_names"`
}

func (h *activitiesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	activities, err := parseActivities(body)
	if err != nil {
		log.WithError(err).Warn("could not parse activities payload")
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if len(activities) == 0 {
		log.Debug("activities payload contained no events")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"no-op"}`))
		return
	}

	processed := 0
	ignored := 0
	for _, a := range activities {
		ids := hostIDsFor(a)
		if len(ids) == 0 {
			ignored++
			log.WithField("type", a.Type).Debug("activity has no inventory impact, ignoring")
			continue
		}
		for _, id := range ids {
			if err := h.dispatch(r.Context(), a, id); err != nil {
				log.WithError(err).WithFields(logrus.Fields{"type": a.Type, "host_id": id}).Error("dispatch failed")
				continue
			}
			processed++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status":"ok","processed":%d,"ignored":%d}`, processed, ignored)
}

// dispatch handles one activity for one host. Most types trigger a re-sync of
// the host via the Fleet detail endpoint; deletion types just log so an
// operator can retire the asset manually (we deliberately don't auto-delete).
func (h *activitiesHandler) dispatch(ctx context.Context, a activity, hostID uint) error {
	switch a.Type {
	case "deleted_host", "deleted_multiple_hosts":
		log.WithFields(logrus.Fields{"type": a.Type, "host_id": hostID}).Warn("host deleted in Fleet — Snipe-IT asset left in place (retire manually if needed)")
		return nil
	}

	host, err := h.fleet.GetHost(ctx, hostID)
	if err != nil {
		if errors.Is(err, fleetapi.ErrNotFound) {
			log.WithField("host_id", hostID).Info("host no longer in Fleet, skipping")
			return nil
		}
		return fmt.Errorf("fetching host %d: %w", hostID, err)
	}
	return h.engine.SyncHost(ctx, *host)
}

// parseActivities decodes the Fleet activities webhook payload. Fleet has used
// both an enveloped form ({"activities": [...]}) and a bare array depending on
// version; we accept either, plus a single object for defensive flexibility.
func parseActivities(body []byte) ([]activity, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, nil
	}
	switch trimmed[0] {
	case '[':
		var arr []activity
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	case '{':
		var env struct {
			Activities []activity `json:"activities"`
		}
		if err := json.Unmarshal(body, &env); err == nil && len(env.Activities) > 0 {
			return env.Activities, nil
		}
		// Single activity object?
		var single activity
		if err := json.Unmarshal(body, &single); err != nil {
			return nil, err
		}
		if single.Type == "" {
			return nil, nil
		}
		return []activity{single}, nil
	default:
		return nil, fmt.Errorf("unexpected payload start: %q", trimmed[:1])
	}
}

// hostIDsFor returns the host IDs an activity should trigger a re-sync for.
// Returns an empty slice for activity types that don't affect Snipe-IT inventory.
func hostIDsFor(a activity) []uint {
	switch a.Type {
	case "enrolled_host", "refetched_host", "mdm_enrolled", "mdm_unenrolled", "deleted_host":
		var d singleHostDetails
		if err := json.Unmarshal(a.Details, &d); err != nil || d.HostID == 0 {
			return nil
		}
		return []uint{d.HostID}
	case "transferred_hosts", "deleted_multiple_hosts":
		var d multiHostDetails
		if err := json.Unmarshal(a.Details, &d); err != nil {
			return nil
		}
		return d.HostIDs
	}
	return nil
}

// authorize compares the configured shared secret to the request's header or
// query parameter using constant-time comparison. When secret is empty, all
// requests are allowed (with a startup warning).
func (h *activitiesHandler) authorize(r *http.Request) bool {
	if h.secret == "" {
		return true
	}
	candidate := r.Header.Get("X-Fleet2Snipe-Secret")
	if candidate == "" {
		candidate = r.URL.Query().Get("secret")
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(h.secret)) == 1
}

// accessLog wraps a handler with structured access logging.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.WithFields(logrus.Fields{
			"method":   r.Method,
			"path":     r.URL.Path,
			"status":   sw.status,
			"duration": time.Since(start).String(),
			"remote":   r.RemoteAddr,
		}).Info("http")
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
