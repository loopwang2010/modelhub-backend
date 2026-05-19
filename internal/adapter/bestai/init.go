package bestai

import (
	"errors"
	"log"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

func init() {
	for _, m := range SeedManifests() {
		if err := catalog.Register(m); err != nil {
			log.Printf("bestai: catalog register %s: %v", m.Key, err)
			continue
		}
		if !m.Enabled {
			if err := catalog.DefaultRegistry.SetEnabled(m.Key, false); err != nil {
				log.Printf("bestai: catalog disable %s: %v", m.Key, err)
			}
		}
	}
}

func BootstrapFromEnv() (bool, error) {
	a, err := NewFromEnv()
	if err != nil {
		if errors.Is(err, adapter.ErrNotConfigured) {
			return false, nil
		}
		return false, err
	}
	if _, err := adapter.DefaultRegistry().Replace(a); err != nil {
		return false, err
	}
	return true, nil
}
