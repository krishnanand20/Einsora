package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestDefaultsAreAValidDevConfig(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "dev" || c.AuthMode != "dev" || c.ClusterBackend != "fake" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.FakeCapacity["l40s-48gb"] != 4 {
		t.Fatalf("fake capacity = %v", c.FakeCapacity)
	}
}

func TestProductionGuards(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"dev auth outside dev", map[string]string{"TENSORA_ENV": "production", "TENSORA_DATABASE_URL": "postgres://x"}, "AUTH_MODE=dev"},
		{"fake cluster in prod", map[string]string{
			"TENSORA_ENV": "production", "TENSORA_DATABASE_URL": "postgres://x",
			"TENSORA_AUTH_MODE": "oidc", "TENSORA_OIDC_ISSUER": "https://auth",
		}, "CLUSTER_BACKEND=fake"},
		{"memory store outside dev", map[string]string{
			"TENSORA_ENV": "staging", "TENSORA_AUTH_MODE": "oidc", "TENSORA_OIDC_ISSUER": "https://auth",
		}, "DATABASE_URL is required"},
		{"oidc without issuer", map[string]string{"TENSORA_AUTH_MODE": "oidc"}, "OIDC_ISSUER"},
		{"bad capacity", map[string]string{"TENSORA_FAKE_CAPACITY": "l40s"}, "FAKE_CAPACITY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromEnv(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidProductionConfig(t *testing.T) {
	_, err := FromEnv(env(map[string]string{
		"TENSORA_ENV": "production", "TENSORA_DATABASE_URL": "postgres://x",
		"TENSORA_AUTH_MODE": "oidc", "TENSORA_OIDC_ISSUER": "https://auth/realms/tensora",
		"TENSORA_CLUSTER_BACKEND": "kubernetes",
	}))
	if err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseURL(t *testing.T) {
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	tests := []struct {
		url, driver, dsn string
		ok               bool
	}{
		{"", "", "", true},
		{"postgres://tensora@db/tensora", "postgres", "postgres://tensora@db/tensora", true},
		{"postgresql://tensora@db/tensora", "postgres", "postgresql://tensora@db/tensora", true},
		{"sqlite:tensora.db", "sqlite", abs("tensora.db"), true},
		{"sqlite:/var/lib/tensora/tensora.db", "sqlite", "/var/lib/tensora/tensora.db", true},
		{"sqlite:///var/lib/tensora/tensora.db", "sqlite", "/var/lib/tensora/tensora.db", true},
		{"sqlite:", "", "", false},
		{"sqlite:x.db?mode=memory", "", "", false},
		{"sqlite::memory:", "", "", false},
		{"sqlite:file:x.db", "", "", false},
		{"mysql://db", "", "", false},
	}
	for _, tt := range tests {
		db, err := Config{DatabaseURL: tt.url}.Database()
		if (err == nil) != tt.ok || db.Driver != tt.driver || db.DSN != tt.dsn {
			t.Errorf("%q → %+v, %v", tt.url, db, err)
		}
	}
	// A SQLite file is a valid store outside dev too (Solo and Lab).
	if _, err := FromEnv(env(map[string]string{
		"TENSORA_ENV": "production", "TENSORA_DATABASE_URL": "sqlite:/var/lib/tensora/tensora.db",
		"TENSORA_AUTH_MODE": "oidc", "TENSORA_OIDC_ISSUER": "https://auth", "TENSORA_CLUSTER_BACKEND": "kubernetes",
	})); err != nil {
		t.Errorf("sqlite in production: %v", err)
	}
	if _, err := FromEnv(env(map[string]string{"TENSORA_DATABASE_URL": "mysql://db"})); err == nil {
		t.Error("an unknown database URL must be rejected at startup")
	}
}

func TestLocalAuthIsAllowedEverywhere(t *testing.T) {
	if _, err := FromEnv(env(map[string]string{
		"TENSORA_ENV": "production", "TENSORA_DATABASE_URL": "sqlite:/var/lib/tensora/tensora.db",
		"TENSORA_AUTH_MODE": "local", "TENSORA_CLUSTER_BACKEND": "kubernetes",
	})); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedProxiesAndPlainHTTP(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"TENSORA_TRUSTED_PROXIES": " 10.0.0.0/8, 127.0.0.1 ,::1, 192.168.1.77/24", "TENSORA_INSECURE_HTTP": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range c.TrustedProxies {
		got = append(got, p.String())
	}
	if want := "10.0.0.0/8 127.0.0.1/32 ::1/128 192.168.1.0/24"; strings.Join(got, " ") != want || !c.InsecureHTTP {
		t.Errorf("proxies = %v, insecure = %v; want %s and true", got, c.InsecureHTTP, want)
	}
	for k, v := range map[string]string{"TENSORA_TRUSTED_PROXIES": "proxy.local", "TENSORA_INSECURE_HTTP": "maybe"} {
		if _, err := FromEnv(env(map[string]string{k: v})); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: %v; want an error naming it", k, v, err)
		}
	}
}

func TestAgentBackend(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"TENSORA_CLUSTER_BACKEND": "agent", "TENSORA_AGENT_DRIVERS": " sim "}))
	if err != nil || c.MachineOwner != "home" || len(c.AgentDrivers) != 1 || c.AgentDrivers[0] != "sim" {
		t.Fatalf("config = %+v, %v", c, err)
	}
	for name, kv := range map[string]map[string]string{
		"sim outside dev": {"TENSORA_CLUSTER_BACKEND": "agent", "TENSORA_AGENT_DRIVERS": "sim", "TENSORA_ENV": "production",
			"TENSORA_DATABASE_URL": "sqlite:/var/lib/tensora/tensora.db", "TENSORA_AUTH_MODE": "local"},
		"an unknown driver":                            {"TENSORA_CLUSTER_BACKEND": "agent", "TENSORA_AGENT_DRIVERS": "kata"},
		"a bad access address":                         {"TENSORA_CLUSTER_BACKEND": "agent", "TENSORA_ACCESS_ADDR": "8090"},
		"an access proxy on the network":               {"TENSORA_CLUSTER_BACKEND": "agent", "TENSORA_ACCESS_ADDR": "0.0.0.0:8090"},
		"an access proxy on a name":                    {"TENSORA_CLUSTER_BACKEND": "agent", "TENSORA_ACCESS_ADDR": "myhost.lan:8090"},
		"a bad owner":                                  {"TENSORA_CLUSTER_BACKEND": "agent", "TENSORA_MACHINE_OWNER": "My Lab"},
		"Postgres, until agents join over the network": {"TENSORA_CLUSTER_BACKEND": "agent", "TENSORA_DATABASE_URL": "postgres://db/tensora"},
		"a relative state directory":                   {"TENSORA_CLUSTER_BACKEND": "agent", "TENSORA_AGENT_STATE_DIR": "state"},
	} {
		if _, err := FromEnv(env(kv)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// macos-native is a real driver: allowed outside development, unlike sim.
func TestNativeDriversOutsideDev(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"TENSORA_ENV": "production", "TENSORA_CLUSTER_BACKEND": "agent",
		"TENSORA_DATABASE_URL": "sqlite:/var/lib/tensora/tensora.db", "TENSORA_AUTH_MODE": "local", "TENSORA_AGENT_DRIVERS": "macos-native"}))
	if err != nil || c.AccessAddr != "127.0.0.1:8090" {
		t.Fatalf("config = %+v, %v", c, err)
	}
}

func TestLoopbackAccessAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8090", "[::1]:9000", "localhost:8090"} {
		if _, err := FromEnv(env(map[string]string{"TENSORA_CLUSTER_BACKEND": "agent", "TENSORA_ACCESS_ADDR": addr})); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}
