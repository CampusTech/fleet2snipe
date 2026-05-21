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

// NewServeCmd returns the `serve` subcommand — an HTTP listener that treats
// Fleet's activities webhook as a wake-up signal. The payload itself is
// ignored beyond extracting host IDs; for each unique host referenced, we GET
// the full host detail from Fleet and reconcile it into Snipe-IT.
func NewServeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "serve",
		Short: "Listen for Fleet activities webhooks and pull fresh host detail",
		Long: `Starts an HTTP server that receives Fleet's activities webhook (Settings → Integrations → Automations → Activities) and uses each event as a wake-up signal to fetch fresh host detail from Fleet's API.

The activity type doesn't drive the data — we just dedupe the host IDs the payload references and pull each one via GET /api/v1/fleet/hosts/{id}, then push into Snipe-IT. This way we automatically benefit from any future Fleet activity type that names a host, without needing to maintain a type allowlist.

Deletions (deleted_host / deleted_multiple_hosts) are the only special case: the host is gone from Fleet so we log the event but leave the Snipe-IT asset in place (retire manually).

Detail drift that osquery surfaces between activity events (free disk space, IP changes, OS minor version) is NOT visible here — Fleet emits no event for it. Run "fleet2snipe sync" on a cron (every 15 min is typical) as your authoritative reconciliation loop.

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

// activity is one entry from the Fleet activity feed. Fleet's wire format for
// the activities webhook posts an envelope with {"activities": [...]} but we
// also accept a bare array or a single object for resilience across versions.
type activity struct {
	ID            uint64          `json:"id"`
	CreatedAt     string          `json:"created_at"`
	ActorFullName string          `json:"actor_full_name"`
	Type          string          `json:"type"`
	Details       json.RawMessage `json:"details"`
}

// deletionTypes are activity types where the host no longer exists in Fleet, so
// pulling its detail will 404. We log and skip rather than treating as an error.
var deletionTypes = map[string]bool{
	"deleted_host":           true,
	"deleted_multiple_hosts": true,
}

// activityDetails is a permissive shape that captures host references regardless
// of the specific activity type. Fleet uses host_id (single) for most events
// and host_ids (plural) for bulk operations like transferred_hosts.
type activityDetails struct {
	HostID  uint   `json:"host_id"`
	HostIDs []uint `json:"host_ids"`
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

	// Collect unique host IDs across the payload. We dedupe so a burst of
	// activities for the same host (e.g. enrolled + mdm_enrolled + installed_software
	// landing together) results in a single API pull.
	hosts := make(map[uint]string) // host_id -> latest activity type seen, for logging
	deleted := 0
	for _, a := range activities {
		var d activityDetails
		if len(a.Details) == 0 || json.Unmarshal(a.Details, &d) != nil {
			continue
		}
		if deletionTypes[a.Type] {
			ids := d.HostIDs
			if d.HostID != 0 {
				ids = append(ids, d.HostID)
			}
			for _, id := range ids {
				log.WithFields(logrus.Fields{"type": a.Type, "host_id": id}).Warn("host deleted in Fleet — Snipe-IT asset left in place (retire manually if needed)")
				deleted++
			}
			continue
		}
		if d.HostID != 0 {
			hosts[d.HostID] = a.Type
		}
		for _, id := range d.HostIDs {
			hosts[id] = a.Type
		}
	}

	processed := 0
	for id, atype := range hosts {
		if err := h.refresh(r.Context(), id); err != nil {
			log.WithError(err).WithFields(logrus.Fields{"trigger": atype, "host_id": id}).Error("refresh failed")
			continue
		}
		processed++
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status":"ok","activities":%d,"hosts_refreshed":%d,"hosts_deleted":%d}`, len(activities), processed, deleted)
}

// refresh pulls fresh detail for a host from Fleet and reconciles into Snipe-IT.
// A 404 from Fleet means the host was deleted between the activity firing and
// our pull — treat as a no-op rather than an error.
func (h *activitiesHandler) refresh(ctx context.Context, hostID uint) error {
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
