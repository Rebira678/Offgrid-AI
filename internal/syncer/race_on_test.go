//go:build race

package syncer

// The race detector instruments allocations, so the ceilings in allocation tests are looser under it.
const allocCeiling = 120
