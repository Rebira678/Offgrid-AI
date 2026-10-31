#!/usr/bin/env bash
# scripts/seed-simple-demo.sh
# Creates clean, simple English data for the Offgrid video recording.
set -euo pipefail

cd "$(dirname "$0")/.."
BIN="./bin/offgrid"
make build >/dev/null

DB="./my-tasks.db"
QDB="./queue.db"
MDIR="./metrics"

echo "=================================================="
echo " 1. Resetting database with simple English tasks"
echo "=================================================="
rm -f "$DB" "$DB-shm" "$DB-wal"

# Create simple English tasks
$BIN exec -db "$DB" '{"intent":"create_task","title":"Fix water leak on pipe 4","site":"site-4","priority":"high"}'
$BIN exec -db "$DB" '{"intent":"create_task","title":"Check battery voltage","site":"site-2","priority":"medium"}'
$BIN exec -db "$DB" '{"intent":"create_task","title":"Clean solar panels","site":"site-1","priority":"low"}'
$BIN exec -db "$DB" '{"intent":"create_task","title":"Replace water filter","site":"site-2","priority":"high"}'
$BIN exec -db "$DB" '{"intent":"create_task","title":"Test backup power generator","site":"site-3","priority":"high"}'
$BIN exec -db "$DB" '{"intent":"create_task","title":"Order spare parts for pump","site":"site-4","priority":"medium"}'
$BIN exec -db "$DB" '{"intent":"create_task","title":"Inspect water tank valve","site":"site-1","priority":"low"}'
$BIN exec -db "$DB" '{"intent":"create_task","title":"Check wifi router antenna","site":"site-2","priority":"medium"}'
$BIN exec -db "$DB" '{"intent":"create_task","title":"Fix broken door lock on shed","site":"site-3","priority":"low"}'
$BIN exec -db "$DB" '{"intent":"create_task","title":"Inspect fuel tank level","site":"site-4","priority":"high"}'

# Add simple notes to every task
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Fix water leak","note":"Found loose fitting near pump. Tightening now."}'
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Fix water leak","note":"Put bucket under pipe. Water flow stopped after valve shut."}'

$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Check battery voltage","note":"Battery reading is 52V. All good."}'
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Check battery voltage","note":"Cleaned dust and dirt off battery terminals."}'

$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Clean solar panels","note":"Dust covering panels after wind storm."}'
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Clean solar panels","note":"Rinsed panel bank 1 with clean water. Power output improved."}'

$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Replace water filter","note":"Filter cartridge dirty from red clay sediment."}'
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Replace water filter","note":"Removed old filter. Waiting for new spare pack to arrive."}'

$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Test backup power generator","note":"Generator started on second try. Testing power load."}'
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Test backup power generator","note":"Ran for 20 minutes under load. Oil and temperature look good."}'

$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Order spare parts for pump","note":"Called supplier. Ordered 2 rubber seals and 1 clamp."}'
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Order spare parts for pump","note":"Delivery scheduled for Tuesday morning."}'

$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Inspect water tank valve","note":"Valve handle was stiff. Put oil on the brass joint."}'
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Inspect water tank valve","note":"Turned valve twice. Shuts off completely with zero drips."}'

$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Check wifi router antenna","note":"Signal dropped in workshop. Found loose antenna wire."}'
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Check wifi router antenna","note":"Tightened antenna cable. Signal back to full bars."}'

$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Fix broken door lock on shed","note":"Key was jammed in the old lock cylinder."}'
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Fix broken door lock on shed","note":"Installed new padlock. Gave spare key to site guard."}'

$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Inspect fuel tank level","note":"Tank is at 35 percent. Need delivery next week."}'
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"Inspect fuel tank level","note":"Dipstick showed 350 liters remaining in main tank."}'

# Update statuses
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"Check battery voltage","status":"done"}'
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"Inspect water tank valve","status":"done"}'
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"Fix broken door lock","status":"done"}'
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"Fix water leak","status":"in_progress"}'
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"Test backup power generator","status":"in_progress"}'

echo ""
echo "=================================================="
echo " 2. Creating AI Copilot Proposals (Offline Model)"
echo "=================================================="
# Proposal 1: Ready to apply (clean emerald accept button in video)
$BIN exec -as-ai -by llama3.2:3b -db "$DB" '{"intent":"update_task","task_ref":"Clean solar panels","status":"in_progress","priority":"medium","note":"Local AI noticed power output dropped 15%. Recommend cleaning panels today."}'

# Proposal 2: Conflict showcase (Remote task updated, locking auto-merge)
$BIN exec -as-ai -by llama3.2:3b -db "$DB" '{"intent":"update_task","task_ref":"Fix water leak","priority":"low"}'
# Someone on site updated the priority to medium after the AI proposed low:
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"Fix water leak","priority":"medium"}'

echo ""
echo "=================================================="
echo " 3. Simulating Peer Devices Sync (P2P Mesh)"
echo "=================================================="
SYNC_KEY="/tmp/offgrid-simple-sync.key"
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$SYNC_KEY"

# Start temporary sync listener
$BIN sync serve -db "$DB" -addr 127.0.0.1:9879 -key-file "$SYNC_KEY" >/dev/null 2>&1 &
SRV_PID=$!
sleep 0.5

# Device 1: field-tablet
T_DB="/tmp/field-tablet.db"; rm -f "$T_DB"
OFFGRID_NODE=field-tablet $BIN exec -db "$T_DB" '{"intent":"create_task","title":"Check emergency lights in warehouse","site":"site-2","priority":"medium"}' >/dev/null 2>&1
OFFGRID_NODE=field-tablet $BIN exec -db "$T_DB" '{"intent":"add_note","task_ref":"Check emergency lights","note":"All 4 battery packs tested and working."}' >/dev/null 2>&1
OFFGRID_NODE=field-tablet $BIN exec -db "$T_DB" '{"intent":"add_note","task_ref":"Check emergency lights","note":"Replaced dead bulb on unit 2."}' >/dev/null 2>&1

OFFGRID_NODE=field-tablet $BIN exec -db "$T_DB" '{"intent":"create_task","title":"Inspect north fence line for damage","site":"site-4","priority":"low"}' >/dev/null 2>&1
OFFGRID_NODE=field-tablet $BIN exec -db "$T_DB" '{"intent":"add_note","task_ref":"Inspect north fence","note":"Found small gap near creek. Patched with wire mesh."}' >/dev/null 2>&1

OFFGRID_NODE=field-tablet $BIN sync once -db "$T_DB" -peer http://127.0.0.1:9879 -key-file "$SYNC_KEY" >/dev/null 2>&1

# Device 2: office-laptop
O_DB="/tmp/office-laptop.db"; rm -f "$O_DB"
OFFGRID_NODE=office-laptop $BIN exec -db "$O_DB" '{"intent":"create_task","title":"Review weekly safety checklist","site":"site-1","priority":"low"}' >/dev/null 2>&1
OFFGRID_NODE=office-laptop $BIN exec -db "$O_DB" '{"intent":"add_note","task_ref":"Review weekly safety","note":"All exit doors clear and fire extinguishers inspected."}' >/dev/null 2>&1
OFFGRID_NODE=office-laptop $BIN exec -db "$O_DB" '{"intent":"add_note","task_ref":"Review weekly safety","note":"Safety sign-off sheet filed in office binder."}' >/dev/null 2>&1
OFFGRID_NODE=office-laptop $BIN exec -db "$O_DB" '{"intent":"update_task","task_ref":"Review weekly safety","status":"done"}' >/dev/null 2>&1

OFFGRID_NODE=office-laptop $BIN exec -db "$O_DB" '{"intent":"create_task","title":"Update spare parts inventory list","site":"site-1","priority":"medium"}' >/dev/null 2>&1
OFFGRID_NODE=office-laptop $BIN exec -db "$O_DB" '{"intent":"add_note","task_ref":"Update spare parts","note":"Counted 8 spare fuses and 4 hose clamps."}' >/dev/null 2>&1

OFFGRID_NODE=office-laptop $BIN sync once -db "$O_DB" -peer http://127.0.0.1:9879 -key-file "$SYNC_KEY" >/dev/null 2>&1

kill $SRV_PID 2>/dev/null || true
rm -f "$SYNC_KEY" "$T_DB" "$O_DB"

echo ""
echo "=================================================="
echo " 4. Populating Worker Queue (18 Done · 2 Active)"
echo "=================================================="
go run ./scripts/seed_queue.go "$QDB"

echo ""
echo "=================================================="
echo " 5. Generating Simple Telemetry & Diagnostics"
echo "=================================================="
go run ./scripts/seed_metrics.go "$MDIR"

echo ""
echo "=================================================="
echo " 6. Seeding 4 AI Copilot Proposals (3 Fresh + 1 Conflict)"
echo "=================================================="
go run ./scripts/seed_proposals.go "$DB"
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"Order spare parts","status":"in_progress"}' >/dev/null 2>&1

echo ""
echo "=================================================="
echo " All simple English data created successfully!"
echo "=================================================="
