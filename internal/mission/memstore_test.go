package mission

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/storetest"
)

// memStore is the in-memory store used by the service tests.
type memStore = MemoryStore

func newMemStore() *memStore { return NewMemoryStore() }

func TestMemStoreConformsToMissionStore(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	storetest.MissionLeases(t, newMemStore(), store.WithTenantID(context.Background(), a), store.WithTenantID(context.Background(), b))
}

var errDBDown = errors.New("injected: database unavailable")
