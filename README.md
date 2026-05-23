# fleet2snipe

Sync device inventory from [Fleet](https://fleetdm.com) into [Snipe-IT](https://snipeitapp.com). Written in Go.

Inspired by [`grokability/jamf2snipe`](https://github.com/grokability/jamf2snipe) — same purpose, but sourced from Fleet (osquery-based, cross-platform) instead of Jamf Pro, with a webhook listener for near-real-time updates and richer mapping options (gjson, policies, saved queries, labels).

## What you get

- **One binary, two modes** — `sync` (full reconciliation, run from cron) and `serve` (HTTP listener for Fleet activity webhooks; pulls one host per event).
- **Five overlapping ways to map data into Snipe-IT custom fields**: gjson paths, policy pass/fail, saved-query result columns, per-label boolean, full label list.
- **Idempotent `setup`** that creates the custom fields in Snipe-IT, associates them with your fieldset, and writes the resulting `field_mapping` back to your `settings.yaml`.
- **Hand-rolled Fleet client** — Bearer auth, paginated listing, `Retry-After`-aware backoff. No `github.com/fleetdm/fleet/v4` import bloat.
- **`michellepellon/go-snipeit`** for Snipe-IT, wrapped with dry-run enforcement and token-bucket rate limiting.
- **Device images** for Apple hardware via [appledb.dev](https://appledb.dev), attached to newly-created Snipe-IT models.
- **`--dry-run`** gated at every mutation; local **cache** for offline dev (`--use-cache`).
- **Custom-field rejection retry** — if Snipe-IT rejects a field for being outside the model's fieldset, fleet2snipe strips it and retries so the rest of the update still lands.
- **Distroless Dockerfile** and sample **systemd unit** included.

## Quick start

```sh
go build ./...

cp settings.example.yaml settings.yaml
$EDITOR settings.yaml                  # fill in Fleet/Snipe credentials + IDs

./fleet2snipe test                     # verify connectivity to both
./fleet2snipe setup                    # create custom fields in Snipe-IT
./fleet2snipe sync --dry-run --verbose # preview
./fleet2snipe sync                     # do it
```

## Authentication

**Fleet** — create an `api_only` user (Settings → Users → Create user → check **API only**), then copy their API token. Dedicate the account: any other login as that user rotates the token.

**Snipe-IT** — Account → Manage API Keys → Create New Token.

Set credentials via `settings.yaml` or env vars: `FLEET_URL`, `FLEET_TOKEN`, `SNIPE_URL`, `SNIPE_API_KEY`, `FLEET2SNIPE_WEBHOOK_SECRET`.

## Two modes, one engine

### `sync` — full reconciliation

```sh
./fleet2snipe sync                            # full sweep
./fleet2snipe sync --force --verbose          # ignore freshness check
./fleet2snipe sync --serial C02XK1JJJG5J      # one host
./fleet2snipe sync --identifier <uuid|hostname|serial|node_key>
./fleet2snipe sync --use-cache                # replay last fetch from .cache/hosts.json
./fleet2snipe sync --update-only              # never create new assets
```

Run on a cron (every 15 min is typical) as your authoritative reconciliation loop. Fleet doesn't emit events when osquery re-reports, so detail drift (free disk space, IPs, OS minor versions) is only caught by polling.

### `serve` — activity-driven wake-ups

```sh
./fleet2snipe serve --verbose
```

In Fleet, **Settings → Integrations → Automations → Activities** webhook, posting to:

```
https://<your-host>:9090/webhook/fleet?secret=<your-webhook-secret>
```

The activity payload is treated as a **wake-up signal**. fleet2snipe extracts every `host_id` it can find in the batch, dedupes, then `GET`s `/api/v1/fleet/hosts/{id}` for each one and reconciles into Snipe-IT.

- No activity-type allowlist — any current or future activity that references a host triggers a refresh.
- A burst (e.g. enrollment + MDM enrolled + software installed for the same host, all landing together) becomes **one** Fleet pull and one Snipe-IT update.
- A 404 from the detail fetch (host deleted mid-flight) is handled silently.
- `deleted_host` / `deleted_multiple_hosts` activities are logged but the Snipe-IT asset is **left in place** — retire manually. We never auto-delete inventory.

Fleet's other automation webhooks (host status, failing policies, vulnerabilities) fire on operational events rather than inventory changes, so we don't subscribe to them.

## How fields get populated

Five sources feed the same `custom_fields` map; values that come back empty are skipped (we never overwrite Snipe-IT data with `""`).

### 1. `field_mapping` — gjson paths into the host JSON

Auto-populated by `fleet2snipe setup`. Each entry is either a bare gjson path or an object with `path` + optional `transform`. Both forms coexist:

```yaml
sync:
  field_mapping:
    _snipeit_fleet_host_id_1: id                # bare string — path only
    _snipeit_fleet_os_version_2: os_version
    _snipeit_mdm_enrollment_3: mdm.enrollment_status
    _snipeit_first_label_4: labels.0.name

    _snipeit_ram_5:                              # object form — adds a transform
      path: memory
      transform: bytes_to_gb                     # 17179869184 bytes → "17"
    _snipeit_storage_6:
      path: gigs_total_disk_space
      transform: gib_to_gb                       # 465.5 GiB → "500"
```

Full [gjson](https://github.com/tidwall/gjson) syntax (arrays, filters, modifiers) is supported on `path`.

**Transforms** standardise units before writing to Snipe-IT. Fleet emits memory as `int64` bytes and disk space as `float` *GiB* (despite the misleading `gigs_*` field name) — without a transform you'd get inconsistent units across the same fieldset. Supported transforms:

| Name           | Input                            | Output                                      |
|----------------|----------------------------------|---------------------------------------------|
| `bytes_to_gb`  | int64 bytes                      | decimal GB (`bytes / 10⁹`), rounded integer |
| `gib_to_gb`    | float GiB                        | decimal GB (`GiB × 1.073741824`), rounded   |

Zero, missing, or unparseable values resolve to `""` so we never clobber real Snipe-IT data with a placeholder from a host that hasn't reported in yet. Unknown transform names are **rejected at config load** with a clear error rather than silently no-op'ing.

### 2. `policy_mapping` — Fleet policies (compliance "controls")

```yaml
sync:
  policy_mapping:
    _snipeit_filevault_10: "FileVault is enabled"
    _snipeit_gatekeeper_11: "Gatekeeper is enabled"
```

Writes `"pass"` / `"fail"` / `""` per host. Free piggyback on the host detail — `populate_policies` is auto-enabled when this map is non-empty.

### 3. `query_mapping` — osquery saved-query results

```yaml
sync:
  query_mapping:
    _snipeit_kernel_version_12:
      query: "Kernel version"
      column: "kernel_version"
    _snipeit_ad_domain_13:
      query: "Joined AD domain"
      column: "domain"
```

The saved query must have **discard_data=false** (i.e. "Save results in Fleet"). Each configured query is fetched **once per sync run** and indexed by `host_id` — a 5,000-host fleet with three query mappings costs 3 API calls, not 15,000.

### 4. `label_mapping` — per-label membership

```yaml
sync:
  label_mapping:
    _snipeit_is_engineering_15: "Engineering laptops"
    _snipeit_is_kiosk_16: "Kiosks"
```

Writes `"yes"` if the host belongs to the named label, `"no"` otherwise. Auto-enables `populate_labels`.

### 5. `labels_field` — full label list

```yaml
sync:
  labels_field: _snipeit_fleet_labels_17
```

A single Snipe-IT field that receives an alphabetised, comma-separated list of every label the host belongs to. Sorted output means a stable membership set produces a stable field value — no PATCH churn.

## Checkout to assigned user

Mirrors `jamf2snipe -u / -ui / -uf` but generalised across whichever Fleet field carries the user identifier. Disabled by default.

```yaml
sync:
  checkout:
    enabled: true
    user_field: "end_users.0.idp_username"  # gjson path into the host JSON
    match_field: "username"                 # snipe field: username | email | employee_num
    mode: "assign"                          # assign | sync | force
```

- `user_field` is any gjson path that resolves to a single string. Good choices: `end_users.0.idp_username` (Fleet Premium with IDP), `end_users.0.email`, or `users.#(type=="regular").username` (first regular OS user from osquery).
- `match_field` is the Snipe-IT user field to look the value up against. Match is case-insensitive.
- `mode`: `assign` only checks out when the asset is currently unassigned (default, like `-u`); `sync` also reassigns when the user differs (like `-ui`); `force` always (re)assigns (like `-uf`).
- All Snipe-IT users are loaded once at warm time and indexed for O(1) lookups, so per-host sync stays cheap regardless of fleet size.
- Reassignments are handled correctly: Snipe-IT's checkout endpoint refuses to overwrite an existing assignment, so we check the asset in first when the desired user differs.
- A Fleet user that has no Snipe-IT counterpart is logged at info and skipped — fleet2snipe never auto-creates users.

## Setup subcommand

`fleet2snipe setup` is **idempotent** and safe to re-run. It creates / updates a baseline set of `Fleet: …` custom fields in Snipe-IT, associates them with your configured fieldset, and rewrites `sync.field_mapping` in `settings.yaml` (preserving comments) with the resulting `db_column_name`s.

**Manual prereqs in Snipe-IT** (one time):

1. Create at least one fieldset → `snipe_it.custom_fieldset_id`. Optionally create one fieldset per platform (e.g. one for macOS, one for Windows, one for mobile) and list them under `snipe_it.fieldset_ids` — `sync` will attach the right fieldset to each auto-created model based on the host's Fleet platform. `setup` associates every `Fleet: …` custom field with every configured fieldset in one idempotent pass.
2. Create a status label for new assets → `snipe_it.default_status_id`.
3. Create one or more model categories (e.g. per OS family) → `snipe_it.category_ids`.

Manufacturers can be left blank — `sync` auto-creates them from Fleet's `hardware_vendor`.

### Per-platform fieldsets

Analogous to jamf2snipe's `computer_custom_fieldset_id` / `mobile_custom_fieldset_id`, but generalised across every Fleet platform:

```yaml
snipe_it:
  custom_fieldset_id: 4           # fallback for platforms not listed below
  fieldset_ids:
    darwin: 5                     # MacBooks / iMacs / Mac minis
    ios: 6                        # iPhones
    ipados: 6                     # iPads share the iOS fieldset
    windows: 7
    linux: 8
    chrome: 9
```

`setup` writes every field to all of those fieldsets. You can then prune fields that don't apply per-platform inside Snipe-IT (e.g. remove `Fleet: Disk Encryption` from the Chrome fieldset).

## Operating notes

- **Match key**: `hardware_serial`. Hosts with no serial are skipped. Two Snipe-IT assets sharing a serial → flagged and skipped to avoid clobbering the wrong record.
- **Freshness check**: a host whose Fleet `detail_updated_at` is older than Snipe-IT's `updated_at` is skipped. Use `--force` (or `sync.force: true`) to ignore.
- **Asset tag**: template-driven. `sync.asset_tag.template` is a string with `{gjson.path}` placeholders interpolated from the Fleet host JSON (e.g. `"CG-{hardware_serial}"`, `"{platform}-{id}"`). `sync.asset_tag.platform_templates` overrides per Fleet platform (mirrors kandji2snipe's per-platform patterns). An explicit empty string asks Snipe-IT to auto-assign (jamf2snipe's `--auto_incrementing`). Legacy `sync.asset_tag_prefix` is still honored as a shortcut for `"{prefix}{id}"`.
- **Model creation**: uses `hardware_model` (e.g. `MacBookPro17,1`) as both the model name and number, attaches the fieldset, and on Apple devices fetches an image from appledb.dev when `sync.model_images: true`.
- **Custom-field rejection retry**: if Snipe-IT rejects fields with "not available on this Asset Model's fieldset", fleet2snipe strips the bad keys and retries once so the rest of the update applies. Re-run `fleet2snipe setup` to fix the underlying fieldset config.
- **Platform filtering**: `sync.platform_filter: ["darwin", "windows"]` to limit which platforms get synced.

## Docker

```sh
docker build -t fleet2snipe .

docker run --rm \
  -e FLEET_URL=https://fleet.example.com \
  -e FLEET_TOKEN=... \
  -e SNIPE_URL=https://snipe.example.com \
  -e SNIPE_API_KEY=... \
  -e FLEET2SNIPE_WEBHOOK_SECRET=... \
  -v $(pwd)/settings.yaml:/app/settings.yaml:ro \
  -p 9090:9090 fleet2snipe serve
```

Or run a one-shot `sync` from a Kubernetes / Cloud Run cron with the same flags.

## Why hand-roll the Fleet client?

`github.com/fleetdm/fleet/v4/server/service` exposes a usable Go client, but importing it drags in the full Fleet server module (MySQL, NanoMDM, AWS SDKs, MaxMind, k8s libs, …). For a small CLI that only needs five endpoints — list hosts, get host, list queries, get query report, list labels — a few hundred lines of `net/http` is the right trade.

## License

MIT
