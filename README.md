# fleet2snipe

Sync device inventory from [Fleet](https://fleetdm.com) into [Snipe-IT](https://snipeitapp.com). Written in Go.

Inspired by [`grokability/jamf2snipe`](https://github.com/grokability/jamf2snipe), but for Fleet (osquery-based) instead of Jamf Pro, with a webhook listener for near-real-time updates.

## Features

- **Two modes in one binary**
  - `fleet2snipe sync` — full reconciliation. Run from cron / Cloud Run / manually.
  - `fleet2snipe serve` — HTTP listener for Fleet automation webhooks. Targets a single host per event, no full scan.
- **Hand-rolled, dependency-light Fleet client** — Bearer auth, pagination, retry on 429/5xx with `Retry-After`.
- **Idempotent Snipe-IT bootstrap** (`setup` subcommand) creates custom fields, associates them with your fieldset, and writes the field mapping back to your config.
- **Configurable field mapping** via [gjson paths](https://github.com/tidwall/gjson) into the Fleet host JSON — extend without touching code.
- **`--dry-run`** writes are gated on every API call that mutates.
- **Local cache** so dev iterations don't hit the Fleet API.
- **Device images** fetched from [appledb.dev](https://appledb.dev) and attached to Apple model records in Snipe-IT (toggle via `sync.model_images`). Non-Apple vendors are skipped cleanly; the lookup table is per-source so new vendor backends can be slotted in.

## Quick start

```sh
go build ./...

cp settings.example.yaml settings.yaml
$EDITOR settings.yaml          # fill in fleet/snipe credentials + IDs

./fleet2snipe test             # verify connectivity
./fleet2snipe setup            # create custom fields in Snipe-IT
./fleet2snipe sync --dry-run --verbose
./fleet2snipe sync
```

## Webhook mode

```sh
./fleet2snipe serve --verbose
```

In Fleet, go to **Settings → Integrations → Automations** and enable the **Activities** webhook pointing at `http(s)://<your-host>:9090/webhook/fleet?secret=<your-secret>`. (Fleet's other webhooks — host status, failing policies, vulnerabilities — fire on operational events, not inventory changes, so we ignore them.)

The activity payload itself is treated as a **wake-up signal**. fleet2snipe extracts every `host_id` referenced anywhere in the batch, dedupes them, then `GET`s the full host detail from Fleet for each one and reconciles into Snipe-IT. This means:

- We don't maintain a type allowlist — any current or future Fleet activity that names a host will trigger a refresh.
- A burst of activities for the same host (enrolled + MDM enrolled + software installed landing together) results in **one** Fleet API call and one Snipe-IT update.
- The 404 case (host was deleted between activity firing and our pull) is handled silently.

`deleted_host` / `deleted_multiple_hosts` are the only special case: we log the event but leave the Snipe-IT asset in place (retire manually).

> **Fleet does not emit per-update webhooks.** Detail changes (OS upgrades, free-disk-space deltas, new IPs) only land in Fleet when osquery re-reports — there's no event for that. Run `fleet2snipe sync` on a cron (every 15 min is typical) as your authoritative reconciliation loop, with `serve` providing the near-real-time path for anything Fleet actually audit-logs.

## Authentication setup

### Fleet

Create an `api_only` user (Settings → Users → Create user, check **API only**) and grab their token from the user dropdown → My Account → Get API Token. Token rotation: any other login by that user invalidates the token, so dedicate the account.

### Snipe-IT

Account → Manage API Keys → Create New Token.

## Setup subcommand

`fleet2snipe setup` is idempotent and safe to re-run. It:

1. Creates / updates the **Fleet:** custom field set in Snipe-IT, scoped to the configured fieldset.
2. Refreshes `sync.field_mapping` in your `settings.yaml` so the sync engine knows where to write each field's value.

Prereqs you do manually in Snipe-IT before running setup:

- Create a fieldset, copy its ID into `snipe_it.custom_fieldset_id`.
- Create a status label, copy its ID into `snipe_it.default_status_id`.
- Create one or more model categories (e.g. one per OS family) and copy IDs into `snipe_it.category_ids`.

Manufacturers can be left blank — `sync` auto-creates them from Fleet's `hardware_vendor` field.

## Operating notes

- **Asset matching** is by `hardware_serial`. Hosts with no serial are skipped. Multiple Snipe-IT assets sharing a serial are flagged and skipped rather than potentially clobbering the wrong record.
- **Freshness check**: by default, a host whose Fleet `detail_updated_at` is older than Snipe-IT's `updated_at` is skipped. Use `--force` or `sync.force: true` to ignore.
- **Model creation**: uses `hardware_model` (e.g. `MacBookPro17,1`) as both the model name and number.
- **Custom-field rejection retry**: if Snipe-IT rejects fields with "not available on this Asset Model's fieldset", fleet2snipe strips them and retries once so the rest of the update still lands. Run `fleet2snipe setup` to fix the underlying fieldset configuration.

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

## License

MIT
