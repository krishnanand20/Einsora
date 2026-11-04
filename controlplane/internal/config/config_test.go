package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		{"dev auth outside dev", map[string]string{"EINSORA_ENV": "production", "EINSORA_DATABASE_URL": "postgres://x"}, "AUTH_MODE=dev"},
		{"fake cluster in prod", map[string]string{
			"EINSORA_ENV": "production", "EINSORA_DATABASE_URL": "postgres://x",
			"EINSORA_AUTH_MODE": "oidc", "EINSORA_OIDC_ISSUER": "https://auth",
		}, "CLUSTER_BACKEND=fake"},
		{"memory store outside dev", map[string]string{
			"EINSORA_ENV": "staging", "EINSORA_AUTH_MODE": "oidc", "EINSORA_OIDC_ISSUER": "https://auth",
		}, "DATABASE_URL is required"},
		{"oidc without issuer", map[string]string{"EINSORA_AUTH_MODE": "oidc"}, "OIDC_ISSUER"},
		{"bad capacity", map[string]string{"EINSORA_FAKE_CAPACITY": "l40s"}, "FAKE_CAPACITY"},
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
		"EINSORA_ENV": "production", "EINSORA_DATABASE_URL": "postgres://x",
		"EINSORA_AUTH_MODE": "oidc", "EINSORA_OIDC_ISSUER": "https://auth/realms/einsora",
		"EINSORA_CLUSTER_BACKEND": "kubernetes",
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
		{"postgres://einsora@db/einsora", "postgres", "postgres://einsora@db/einsora", true},
		{"postgresql://einsora@db/einsora", "postgres", "postgresql://einsora@db/einsora", true},
		{"sqlite:einsora.db", "sqlite", abs("einsora.db"), true},
		{"sqlite:/var/lib/einsora/einsora.db", "sqlite", "/var/lib/einsora/einsora.db", true},
		{"sqlite:///var/lib/einsora/einsora.db", "sqlite", "/var/lib/einsora/einsora.db", true},
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
		"EINSORA_ENV": "production", "EINSORA_DATABASE_URL": "sqlite:/var/lib/einsora/einsora.db",
		"EINSORA_AUTH_MODE": "oidc", "EINSORA_OIDC_ISSUER": "https://auth", "EINSORA_CLUSTER_BACKEND": "kubernetes",
	})); err != nil {
		t.Errorf("sqlite in production: %v", err)
	}
	if _, err := FromEnv(env(map[string]string{"EINSORA_DATABASE_URL": "mysql://db"})); err == nil {
		t.Error("an unknown database URL must be rejected at startup")
	}
}

func TestLocalAuthIsAllowedEverywhere(t *testing.T) {
	if _, err := FromEnv(env(map[string]string{
		"EINSORA_ENV": "production", "EINSORA_DATABASE_URL": "sqlite:/var/lib/einsora/einsora.db",
		"EINSORA_AUTH_MODE": "local", "EINSORA_CLUSTER_BACKEND": "kubernetes",
	})); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedProxiesAndPlainHTTP(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"EINSORA_TRUSTED_PROXIES": " 10.0.0.0/8, 127.0.0.1 ,::1, 192.168.1.77/24", "EINSORA_INSECURE_HTTP": "true",
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
	for k, v := range map[string]string{"EINSORA_TRUSTED_PROXIES": "proxy.local", "EINSORA_INSECURE_HTTP": "maybe"} {
		if _, err := FromEnv(env(map[string]string{k: v})); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: %v; want an error naming it", k, v, err)
		}
	}
}

func TestAgentBackend(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_AGENT_DRIVERS": " sim "}))
	if err != nil || c.MachineOwner != "home" || len(c.AgentDrivers) != 1 || c.AgentDrivers[0] != "sim" {
		t.Fatalf("config = %+v, %v", c, err)
	}
	for name, kv := range map[string]map[string]string{
		"sim outside dev": {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_AGENT_DRIVERS": "sim", "EINSORA_ENV": "production",
			"EINSORA_DATABASE_URL": "sqlite:/var/lib/einsora/einsora.db", "EINSORA_AUTH_MODE": "local"},
		"an unknown driver":                            {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_AGENT_DRIVERS": "kata"},
		"a bad access address":                         {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_ACCESS_ADDR": "8090"},
		"an access proxy on the network":               {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_ACCESS_ADDR": "0.0.0.0:8090"},
		"an access proxy on a name":                    {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_ACCESS_ADDR": "myhost.lan:8090"},
		"a bad owner":                                  {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_MACHINE_OWNER": "My Lab"},
		"Postgres, until agents join over the network": {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_DATABASE_URL": "postgres://db/einsora"},
		"a relative state directory":                   {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_AGENT_STATE_DIR": "state"},
		"a relative run directory":                     {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_AGENT_RUN_DIR": "run"},
		"a remote Docker socket":                       {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_DOCKER_SOCKET": "tcp://host:2375"},
		"a relative Docker socket":                     {"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_DOCKER_SOCKET": "docker.sock"},
	} {
		if _, err := FromEnv(env(kv)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// macos-native is a real driver: allowed outside development, unlike sim.
func TestNativeDriversOutsideDev(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"EINSORA_ENV": "production", "EINSORA_CLUSTER_BACKEND": "agent",
		"EINSORA_DATABASE_URL": "sqlite:/var/lib/einsora/einsora.db", "EINSORA_AUTH_MODE": "local", "EINSORA_AGENT_DRIVERS": "macos-native"}))
	if err != nil || c.AccessAddr != "127.0.0.1:8090" {
		t.Fatalf("config = %+v, %v", c, err)
	}
}

func TestDockerOutsideDev(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"EINSORA_ENV": "production", "EINSORA_CLUSTER_BACKEND": "agent",
		"EINSORA_DATABASE_URL": "sqlite:/var/lib/einsora/einsora.db", "EINSORA_AUTH_MODE": "local",
		"EINSORA_AGENT_DRIVERS": "docker", "EINSORA_DOCKER_SOCKET": "/var/run/docker.sock"}))
	if err != nil || len(c.AgentDrivers) != 1 || c.AgentDrivers[0] != "docker" || c.DockerSocket != "/var/run/docker.sock" {
		t.Fatalf("Docker config = %+v, %v", c, err)
	}
}

func TestLoopbackAccessAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8090", "[::1]:9000", "localhost:8090"} {
		if _, err := FromEnv(env(map[string]string{"EINSORA_CLUSTER_BACKEND": "agent", "EINSORA_ACCESS_ADDR": addr})); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

// Workload output is kept 14 days and 64 MiB per workload unless set otherwise, within bounds.
func TestOutputLimits(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil || c.OutputRetention != 14*24*time.Hour || c.OutputMaxMiB != 64 {
		t.Fatalf("defaults: %v, %v, %v", c.OutputRetention, c.OutputMaxMiB, err)
	}
	c, err = FromEnv(env(map[string]string{"EINSORA_OUTPUT_RETENTION": "720h", "EINSORA_OUTPUT_MAX_MIB": "256"}))
	if err != nil || c.OutputRetention != 720*time.Hour || c.OutputMaxMiB != 256 {
		t.Fatalf("set: %v, %v, %v", c.OutputRetention, c.OutputMaxMiB, err)
	}
	for _, bad := range []map[string]string{
		{"EINSORA_OUTPUT_RETENTION": "30m"}, {"EINSORA_OUTPUT_RETENTION": "a week"},
		{"EINSORA_OUTPUT_MAX_MIB": "0"}, {"EINSORA_OUTPUT_MAX_MIB": "257"}, {"EINSORA_OUTPUT_MAX_MIB": "64MiB"},
	} {
		if _, err := FromEnv(env(bad)); err == nil || !strings.Contains(err.Error(), "EINSORA_OUTPUT_") {
			t.Errorf("%v: err = %v", bad, err)
		}
	}
}
