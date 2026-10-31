#!/usr/bin/env bash
# scripts/populate-demo-data.sh
# Populates senior-level operational data into offgrid for video recording & live demo.
set -euo pipefail

cd "$(dirname "$0")/.."
BIN="./bin/offgrid"
make build >/dev/null

DB="${1:-./my-tasks.db}"
echo "==> Seeding senior-level operational data into $DB..."

# 1. Add realistic edge infrastructure tasks
$BIN exec -db "$DB" '{"intent":"create_task","title":"Verify failover power routing on generator transfer switch","site":"grid-junction-03","priority":"high"}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"create_task","title":"Calibrate borehole water pressure transducer #2","site":"hydro-site-01","priority":"high"}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"create_task","title":"Inspect solar panel array B junction thermal hotspot","site":"solar-array-02","priority":"medium"}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"create_task","title":"Flush write-ahead buffer on telemetry cache node","site":"sat-uplink-01","priority":"low"}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"create_task","title":"Replace 48V LiFePO4 rack battery cell module C4","site":"substation-beta","priority":"high"}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"create_task","title":"Test LoRaWAN relay directional antenna VSWR ratio","site":"relay-ridge-4","priority":"medium"}' >/dev/null 2>&1 || true

# Add engineering field notes
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"generator transfer switch","note":"Transfer switch relay contacts inspected and cleaned; manual cycle test passed."}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"borehole water pressure","note":"Baseline static head reading: 4.8 bar. Transducer zero-offset calibrated."}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"battery cell module","note":"Thermographic scan showed 11°C delta on cell C4-08 under 80A continuous discharge."}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"add_note","task_ref":"directional antenna","note":"VSWR measured 1.18:1 at 868.1 MHz. Return loss is -22dB."}' >/dev/null 2>&1 || true

# Update statuses
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"generator transfer switch","status":"in_progress"}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"solar panel array B","status":"done"}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"battery cell module","status":"in_progress"}' >/dev/null 2>&1 || true

# 2. AI Copilot Proposals
# Fresh proposal (Ready to Accept in video recording)
$BIN exec -as-ai -by llama3.2:3b -db "$DB" '{"intent":"update_task","task_ref":"Calibrate MPPT charge controller #3","status":"in_progress","priority":"high","note":"Local inference detected 14.2V bus float anomaly from telemetry buffer. Staged calibration protocol."}' >/dev/null 2>&1 || true

# Conflict proposal (Demonstrating CRDT safety lock)
$BIN exec -as-ai -by llama3.2:3b -db "$DB" '{"intent":"update_task","task_ref":"Deploy production release","priority":"low"}' >/dev/null 2>&1 || true
$BIN exec -db "$DB" '{"intent":"update_task","task_ref":"Deploy production release","priority":"medium"}' >/dev/null 2>&1 || true

# 3. P2P Replication & Inbound Commits Stream
echo "==> Simulating peer nodes sync (field-tablet-alpha and substation-edge-04)..."
SYNC_KEY="/tmp/offgrid-seed-sync.key"
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$SYNC_KEY"

# Start temporary sync server on local db
$BIN sync serve -db "$DB" -addr 127.0.0.1:9877 -key-file "$SYNC_KEY" >/dev/null 2>&1 &
SRV_PID=$!
sleep 0.5

# Peer 1: field-tablet-alpha
P1="/tmp/peer1.db"; rm -f "$P1"
OFFGRID_NODE=field-tablet-alpha $BIN exec -seed -db "$P1" '{"intent":"list_tasks"}' >/dev/null 2>&1
OFFGRID_NODE=field-tablet-alpha $BIN exec -db "$P1" '{"intent":"create_task","title":"Emergency fuel cutoff valve maintenance","site":"generator-01","priority":"high"}' >/dev/null 2>&1
OFFGRID_NODE=field-tablet-alpha $BIN exec -db "$P1" '{"intent":"add_note","task_ref":"Emergency fuel cutoff","note":"Replaced Teflon gasket on solenoid seat."}' >/dev/null 2>&1
OFFGRID_NODE=field-tablet-alpha $BIN sync once -db "$P1" -peer http://127.0.0.1:9877 -key-file "$SYNC_KEY" >/dev/null 2>&1

# Peer 2: substation-edge-04
P2="/tmp/peer2.db"; rm -f "$P2"
OFFGRID_NODE=substation-edge-04 $BIN exec -seed -db "$P2" '{"intent":"list_tasks"}' >/dev/null 2>&1
OFFGRID_NODE=substation-edge-04 $BIN exec -db "$P2" '{"intent":"create_task","title":"Synchronize RTU telemetry packet timestamp","site":"substation-beta","priority":"medium"}' >/dev/null 2>&1
OFFGRID_NODE=substation-edge-04 $BIN exec -db "$P2" '{"intent":"update_task","task_ref":"Synchronize RTU telemetry","status":"done"}' >/dev/null 2>&1
OFFGRID_NODE=substation-edge-04 $BIN sync once -db "$P2" -peer http://127.0.0.1:9877 -key-file "$SYNC_KEY" >/dev/null 2>&1

kill $SRV_PID 2>/dev/null || true
rm -f "$SYNC_KEY" "$P1" "$P2"

# 4. Generate Telemetry & Diagnostics snapshots
echo "==> Generating telemetry & diagnostics in ./metrics..."
go run ./scripts/seed_metrics.go ./metrics

# 5. Populate queue.db
echo "==> Populating queue.db..."
QDB="./queue.db"
$BIN queue -q "$QDB" enqueue -key sync-p2p-01 -kind sync_outbox '{"peer":"field-tablet-alpha","ops":4}' >/dev/null 2>&1 || true
$BIN queue -q "$QDB" enqueue -key model-eval-02 -kind deep_think '{"target":"battery-degradation"}' >/dev/null 2>&1 || true

echo "==> Senior-level data seeding complete!"
