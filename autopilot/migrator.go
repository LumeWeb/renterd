package autopilot

import (
	"sync"

	"go.uber.org/zap"
)

const (
	migratorBatchSize   = 100
	migratorContractset = "autopilot"
)

type migrator struct {
	ap     *Autopilot
	logger *zap.SugaredLogger

	mu      sync.Mutex
	running bool
}

func newMigrator(ap *Autopilot) *migrator {
	return &migrator{
		ap:     ap,
		logger: ap.logger.Named("migrator"),
	}
}

func (m *migrator) TryPerformMigrations() {
	m.logger.Info("try performing migrations")
	m.mu.Lock()
	if m.running {
		m.logger.Info("migrations still running")
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()

	go func() {
		m.performMigrations()
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()
}

func (m *migrator) performMigrations() {
	m.logger.Info("performing migrations")
	b := m.ap.bus

	for {
		// fetch slabs for migration
		toMigrate, err := b.SlabsForMigration(migratorContractset, migratorBatchSize)
		if err != nil {
			m.logger.Errorf("failed to fetch slabs for migration, err: %v", err)
			return
		}
		m.logger.Debugf("%d slabs to migrate", len(toMigrate))

		// escape early if there's no slabs to migrate
		if len(toMigrate) == 0 {
			return
		}

		// migrate them one by one
		for i, slab := range toMigrate {
			err := m.ap.worker.MigrateSlab(slab)
			if err != nil {
				m.logger.Errorf("failed to migrate slab %d/%d, err: %v", i+1, len(toMigrate), err)
				continue
			}
			m.logger.Debugf("successfully migrated slab %d/%d", i+1, len(toMigrate))
		}
	}
}
