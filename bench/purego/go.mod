module offgrid/bench/purego

go 1.22

require github.com/mattn/go-sqlite3 v1.14.22

// The pure-Go port is only built with -tags modernc. Add it with:
//   go get modernc.org/sqlite@latest && go mod tidy
// (kept out of the main module on purpose: the product stays on mattn.)
