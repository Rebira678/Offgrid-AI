//go:build race

package sqlstore

// Under -race sync.Pool deliberately drops entries, so allocation counts are higher and noisy.
const hashAllocCeiling = 40
