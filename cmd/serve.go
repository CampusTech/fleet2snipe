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
// that accepts Fleet automation webhooks (host status, failing policies,
// vulnerabilities) and reconciles affected hosts into Snipe-IT in near-real-time.
func NewServeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run as an HTTP webhook listener for Fleet automations",
		Long: `Starts an HTTP server that accepts Fleet automation webhooks and reconciles affected hosts into Snipe-IT.

Configure Fleet automations to POST to <addr><path> (default ":9090/webhook/fleet"). Supported payload shapes:
  - Host status webhook ({"hosts": [{...}]})
  - Failing policies webhook ({"failing_policies": [...]})
  - Vulnerabilities webhook
For each host referenced in the payload, fleet2snipe fetches the latest detail via the Fleet API and syncs it.

A POST to <path> from any source must include the configured shared secret in the X-Fleet2Snipe-Secret header (or ?secret= query string). GET <path>/healthz returns 200 OK.`,
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
	h := &webhookHandler{
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

	log.Infof("fleet2snipe webhook server listening on %s%s", addr, path)
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

// webhookHandler serves POSTs from Fleet's automation system.
type webhookHandler struct {
	engine *f2sync.Engine
	fleet  *fleetapi.Client
	secret string
}

// Fleet's webhook payloads vary by automation type. We accept the union of
// shapes and pull out anything that looks like a host identifier.
type fleetWebhookPayload struct {
	Timestamp string `json:"timestamp"`
	// Host status webhook
	Hosts []struct {
		ID             uint   `json:"id"`
		Hostname       string `json:"hostname"`
		DisplayName    string `json:"display_name"`
		HardwareSerial string `json:"hardware_serial"`
	} `json:"hosts"`
	// Failing-policy webhook
	FailingPolicies []struct {
		PolicyID uint `json:"policy_id"`
		Hosts    []struct {
			ID             uint   `json:"id"`
			Hostname       string `json:"hostname"`
			HardwareSerial string `json:"hardware_serial"`
		} `json:"hosts"`
	} `json:"failing_policies"`
	// Vulnerability webhook — Fleet sends host IDs/serials under "hosts_affected".
	HostsAffected []struct {
		ID             uint   `json:"id"`
		Hostname       string `json:"hostname"`
		HardwareSerial string `json:"hardware_serial"`
	} `json:"hosts_affected"`
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	var p fleetWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		log.WithError(err).Warn("could not parse webhook payload")
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	hostIDs := collectHostIDs(p)
	if len(hostIDs) == 0 {
		log.Warn("webhook payload had no host references")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"no-op"}`))
		return
	}

	// Sync each referenced host. Use the request context so SIGTERM aborts.
	ctx := r.Context()
	for _, id := range hostIDs {
		host, err := h.fleet.GetHost(ctx, id)
		if err != nil {
			log.WithError(err).WithField("host_id", id).Error("could not fetch host")
			continue
		}
		if err := h.engine.SyncHost(ctx, *host); err != nil {
			log.WithError(err).WithField("host_id", id).Error("could not sync host")
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status":"ok","hosts_processed":%d}`, len(hostIDs))
}

// authorize compares the configured shared secret to the request's header or
// query parameter using constant-time comparison. When secret is empty, all
// requests are allowed (with a warning logged at startup).
func (h *webhookHandler) authorize(r *http.Request) bool {
	if h.secret == "" {
		return true
	}
	candidate := r.Header.Get("X-Fleet2Snipe-Secret")
	if candidate == "" {
		candidate = r.URL.Query().Get("secret")
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(h.secret)) == 1
}

// collectHostIDs dedupes the host IDs referenced anywhere in the payload.
func collectHostIDs(p fleetWebhookPayload) []uint {
	seen := make(map[uint]struct{})
	for _, h := range p.Hosts {
		if h.ID != 0 {
			seen[h.ID] = struct{}{}
		}
	}
	for _, fp := range p.FailingPolicies {
		for _, h := range fp.Hosts {
			if h.ID != 0 {
				seen[h.ID] = struct{}{}
			}
		}
	}
	for _, h := range p.HostsAffected {
		if h.ID != 0 {
			seen[h.ID] = struct{}{}
		}
	}
	out := make([]uint, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
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
