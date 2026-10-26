package store_test

import (
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// storetest imports store, so this package's own (package store) tests cannot
// import it. This external test file can, and it hands them OpenFresh before
// any test runs: one helper, reached from both sides of the cycle.
func init() { store.OpenFresh = storetest.OpenFresh }
