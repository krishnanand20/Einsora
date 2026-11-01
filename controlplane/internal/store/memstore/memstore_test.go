package memstore_test

import (
	"testing"

	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/store/memstore"
	"github.com/krishnanand20/einsora/controlplane/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return memstore.New() })
}
