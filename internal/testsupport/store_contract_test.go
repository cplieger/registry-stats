package testsupport_test

import (
	"testing"

	"registry-stats/internal/api"
	"registry-stats/internal/testsupport"
)

// TestRunStoreContract_compiles_and_runs is a smoke test ensuring the
// contract helper itself is exercisable. The real value comes from
// store_test.go (store.FS) and webapi_test.go (memStore) calling it.
func TestRunStoreContract_compiles_and_runs(t *testing.T) {
	testsupport.RunStoreContract(t, func(t *testing.T) api.Store {
		return testsupport.NewContractMemStore()
	})
}
