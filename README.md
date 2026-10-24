# Inventra

Offline inventory management for Windows — a single `.exe`, no installation, no internet, no monthly fees. Your data stays on your own computer.

## Download

Grab `Inventra.exe` from the [Releases](../../releases) page and double-click it. It opens in its own window.

> First launch: Windows may show *"Windows protected your PC"* — click **More info → Run anyway**.

## Features

**Items**
- Single items and composite kits (bill of materials)
- SKU, barcode (generate or scan), category, brand, unit, storage location/bin, supplier
- Cost price, reorder level and reorder quantity, opening stock per warehouse
- Batch/lot & expiry tracking, serial numbers
- CSV import / export, barcode label printing (Code 128)

**Stock**
- **Stock In** — goods received, with source, cost, batch and expiry (prints as a GRN)
- **Stock Out** — goods issued, with recipient and purpose (prints as a gate pass)
- **Adjustments** — damage, loss, write-off, revaluation
- **Transfer orders** between warehouses (with in-transit status)
- **Stock counts** — physical stock-taking with barcode scanning; differences are adjusted automatically
- **Assemblies** — build composite items from components
- Multiple warehouses, weighted-average cost valuation, negative-stock warnings

**Reports** (all printable / PDF and exportable to CSV)
Inventory summary · Stock by warehouse · Valuation as of any date · Low stock / reorder · Batches & expiry · Opening/In/Out/Closing · Item ledger · Stock In / Stock Out summaries · Adjustments summary · Inventory aging · Slow-moving & dead stock · ABC analysis · Activity log

**Other**
- Users and roles: Admin, Manager, Staff, Viewer
- Dashboard, global search (Ctrl+K), dark mode
- Automatic daily backups (last 30 days kept), backup download and restore
- Optional multi-PC use over the office network

## Your data

Everything is stored in an `Inventra Data` folder next to `Inventra.exe`:

```
Inventra Data/
  inventra-data.json   ← all your data
  backups/             ← automatic daily copies
```

To move to another PC, copy `Inventra.exe` **and** the `Inventra Data` folder.

## How it works

Inventra is a small Go program with the web interface embedded inside it. When you open it, it starts a local server on `127.0.0.1` and shows the app in an Edge/Chrome "app mode" window (no tabs or address bar). Closing the window shuts it down.

```
main.go               server, storage, logins, backups
launcher_windows.go   opens the app window on Windows
web/                  the interface (HTML, CSS, plain JavaScript — no frameworks)
  js/engine.js        stock ledger and valuation
```

## Building from source

Requires [Go 1.24+](https://go.dev/dl/).

```bash
# Windows .exe (no console window)
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o Inventra.exe .

# Run locally on any OS for development (then open http://127.0.0.1:47821)
go run . -nobrowser -data ./dev-data
```
