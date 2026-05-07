package adapter

import (
	"testing"
)

func TestIsDevMode(t *testing.T) {
	t.Setenv(DevModeEnvVar, "")
	if IsDevMode() {
		t.Error("IsDevMode true with empty env")
	}
	t.Setenv(DevModeEnvVar, "production")
	if IsDevMode() {
		t.Error("IsDevMode true for non-mock value")
	}
	t.Setenv(DevModeEnvVar, DevModeValue)
	if !IsDevMode() {
		t.Error("IsDevMode false with DEV_MODE=mock")
	}
}

func TestRegisterDevModeMocks_PopulatesRegistry(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterDevModeMocks(reg); err != nil {
		t.Fatal(err)
	}
	for _, key := range []ProviderKey{"mock-sync", "mock-async"} {
		a, ok := reg.Get(key)
		if !ok {
			t.Errorf("missing %q after RegisterDevModeMocks", key)
			continue
		}
		if a.Key() != key {
			t.Errorf("%q registered under wrong key %q", key, a.Key())
		}
	}
}

func TestRegisterDevModeMocks_Idempotent(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterDevModeMocks(reg); err != nil {
		t.Fatal(err)
	}
	// Second call must not error (Replace semantics).
	if err := RegisterDevModeMocks(reg); err != nil {
		t.Fatalf("second RegisterDevModeMocks: %v", err)
	}
	if reg.Len() != 2 {
		t.Errorf("expected 2 adapters after idempotent re-register, got %d", reg.Len())
	}
}

func TestRegisterDevModeMocks_NilRegistry(t *testing.T) {
	if err := RegisterDevModeMocks(nil); err == nil {
		t.Error("expected error on nil registry")
	}
}

func TestMaybeBootstrapDevMode_NoopWhenDisabled(t *testing.T) {
	t.Setenv(DevModeEnvVar, "")
	// Snapshot any pre-existing mock-sync registration so we restore it.
	prevSync, _ := defaultRegistry.Get("mock-sync")
	prevAsync, _ := defaultRegistry.Get("mock-async")
	defaultRegistry.Unregister("mock-sync")
	defaultRegistry.Unregister("mock-async")
	t.Cleanup(func() {
		if prevSync != nil {
			defaultRegistry.Replace(prevSync)
		}
		if prevAsync != nil {
			defaultRegistry.Replace(prevAsync)
		}
	})
	bootstrapped, err := MaybeBootstrapDevMode()
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapped {
		t.Error("MaybeBootstrapDevMode returned true with DEV_MODE unset")
	}
	if _, ok := defaultRegistry.Get("mock-sync"); ok {
		t.Error("mock-sync registered despite DEV_MODE unset")
	}
}

func TestMaybeBootstrapDevMode_RegistersWhenEnabled(t *testing.T) {
	t.Setenv(DevModeEnvVar, DevModeValue)
	prevSync, _ := defaultRegistry.Get("mock-sync")
	prevAsync, _ := defaultRegistry.Get("mock-async")
	t.Cleanup(func() {
		if prevSync != nil {
			defaultRegistry.Replace(prevSync)
		} else {
			defaultRegistry.Unregister("mock-sync")
		}
		if prevAsync != nil {
			defaultRegistry.Replace(prevAsync)
		} else {
			defaultRegistry.Unregister("mock-async")
		}
	})
	bootstrapped, err := MaybeBootstrapDevMode()
	if err != nil {
		t.Fatal(err)
	}
	if !bootstrapped {
		t.Fatal("expected bootstrapped=true")
	}
	if _, ok := defaultRegistry.Get("mock-sync"); !ok {
		t.Error("mock-sync not registered")
	}
	if _, ok := defaultRegistry.Get("mock-async"); !ok {
		t.Error("mock-async not registered")
	}
}
