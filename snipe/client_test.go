package snipe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	snipeit "github.com/michellepellon/go-snipeit"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
)

func newTestClient(t *testing.T, mux *http.ServeMux) *Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "test-token", false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// Snipe-IT returns HTTP 200 with status "error" and message "That asset is
// already checked in." when checking in an unassigned asset. The engine calls
// CheckinAsset defensively before reassigning, so that must count as success.
func TestCheckinAssetToleratesAlreadyCheckedIn(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/hardware/434/checkin", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{
			"status": "error",
			"messages": "That asset is already checked in.",
			"payload": {"asset_tag": "AT-434", "model": "MacBook Air 13\" M2", "model_number": "A2681"}
		}`)
	})
	c := newTestClient(t, mux)

	if err := c.CheckinAsset(context.Background(), 434); err != nil {
		t.Errorf("CheckinAsset returned error for already-checked-in asset, want nil: %v", err)
	}
}

// When Snipe-IT rejects custom fields, PatchAsset retries without them and the
// warning should carry the asset's real model so 'fleet2snipe setup' can be
// aimed at the right fieldset — not the zero Model of the patch request.
func TestPatchAssetRetriesWithoutRejectedFields(t *testing.T) {
	patchCalls := 0
	var retryBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/hardware/959", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			patchCalls++
			if patchCalls == 1 {
				_, _ = fmt.Fprint(w, `{
					"status": "error",
					"messages": {"_snipeit_mac_address_1": ["The value you provided is not available on this Asset Model's fieldset."]}
				}`)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&retryBody)
			_, _ = fmt.Fprint(w, `{"status": "success", "payload": {"id": 959}}`)
		case http.MethodGet:
			_, _ = fmt.Fprint(w, `{"id": 959, "asset_tag": "AT-959", "model": {"id": 56, "name": "iPhone 15 Plus"}}`)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	})
	c := newTestClient(t, mux)

	hook := logrustest.NewLocal(log)
	defer hook.Reset()

	patch := snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: map[string]string{
		"_snipeit_mac_address_1": "1c:e2:09:5e:d6:89",
		"_snipeit_storage_3":     "137",
	}}}
	if _, err := c.PatchAsset(context.Background(), 959, patch); err != nil {
		t.Fatalf("PatchAsset: %v", err)
	}

	if patchCalls != 2 {
		t.Fatalf("patch calls = %d, want 2 (initial + retry)", patchCalls)
	}
	if _, ok := retryBody["_snipeit_mac_address_1"]; ok {
		t.Error("retry body still contains rejected field _snipeit_mac_address_1")
	}
	if _, ok := retryBody["_snipeit_storage_3"]; !ok {
		t.Error("retry body lost surviving field _snipeit_storage_3")
	}

	var warned *logrus.Entry
	for _, e := range hook.AllEntries() {
		if e.Level == logrus.WarnLevel {
			warned = e
			break
		}
	}
	if warned == nil {
		t.Fatal("expected a warning about rejected custom fields")
	}
	if got := warned.Data["model_id"]; got != 56 {
		t.Errorf("warning model_id = %v, want 56 (the asset's real model)", got)
	}
	if got := warned.Data["model_name"]; got != "iPhone 15 Plus" {
		t.Errorf("warning model_name = %v, want %q", got, "iPhone 15 Plus")
	}
}
