package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load defaults failed: %v", err)
	}

	if cfg.Server.Port != 8191 {
		t.Fatalf("default port = %d, want 8191", cfg.Server.Port)
	}
	if cfg.Server.Mode != "release" {
		t.Fatalf("default mode = %q, want release", cfg.Server.Mode)
	}
	if cfg.Storage.Bucket != "drop-data" {
		t.Fatalf("default bucket = %q, want drop-data", cfg.Storage.Bucket)
	}
	if cfg.Log.Format != "json" {
		t.Fatalf("default log format = %q, want json", cfg.Log.Format)
	}
	if cfg.Security.Environment != "development" || !cfg.Security.AllowInsecureTransport {
		t.Fatalf("default security config = %+v", cfg.Security)
	}
	if !cfg.Observability.MetricsEnabled {
		t.Fatalf("metrics should be enabled by default")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("PG_DSN", "host=db user=test dbname=drop sslmode=disable")
	t.Setenv("DROP_GRPC", "drop-server:50051")
	t.Setenv("S3_ENDPOINT", "minio:9000")
	t.Setenv("S3_PUBLIC_ENDPOINT", "localhost:9000")
	t.Setenv("S3_ACCESS_KEY", "ak")
	t.Setenv("S3_SECRET_KEY", "sk")
	t.Setenv("PORT", "18888")
	t.Setenv("MINI_DROP_ENV", "production")
	t.Setenv("ALLOW_INSECURE_TRANSPORT", "true")
	t.Setenv("GRPC_MTLS_CERT_FILE", "/certs/client.crt")
	t.Setenv("GRPC_MTLS_KEY_FILE", "/certs/client.key")
	t.Setenv("GRPC_MTLS_CA_FILE", "/certs/ca.crt")
	t.Setenv("METRICS_ENABLED", "false")
	t.Setenv("AGENT_DISCOVERY_EXTRA_IPS", "111.230.29.115,10.0.0.9")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with env failed: %v", err)
	}

	if cfg.Database.DSN != "host=db user=test dbname=drop sslmode=disable" {
		t.Fatalf("database dsn was not overridden: %q", cfg.Database.DSN)
	}
	if cfg.GRPC.Addr != "drop-server:50051" {
		t.Fatalf("grpc addr = %q", cfg.GRPC.Addr)
	}
	if cfg.Storage.Endpoint != "minio:9000" || cfg.Storage.PublicEndpoint != "localhost:9000" {
		t.Fatalf("storage endpoints = %q/%q", cfg.Storage.Endpoint, cfg.Storage.PublicEndpoint)
	}
	if cfg.Storage.AccessKey != "ak" || cfg.Storage.SecretKey != "sk" {
		t.Fatalf("storage credentials were not overridden")
	}
	if cfg.Server.Port != 18888 {
		t.Fatalf("server port = %d, want 18888", cfg.Server.Port)
	}
	if cfg.Security.Environment != "production" || !cfg.Security.AllowInsecureTransport {
		t.Fatalf("security env overrides failed: %+v", cfg.Security)
	}
	if cfg.GRPC.MTLSCertFile != "/certs/client.crt" || cfg.GRPC.MTLSKeyFile != "/certs/client.key" || cfg.GRPC.MTLSCAFile != "/certs/ca.crt" {
		t.Fatalf("mTLS paths not overridden: %+v", cfg.GRPC)
	}
	if cfg.Observability.MetricsEnabled {
		t.Fatalf("metrics override should disable metrics")
	}
	if cfg.AgentDiscovery.ExtraIPs != "111.230.29.115,10.0.0.9" {
		t.Fatalf("agent discovery extra ips = %q", cfg.AgentDiscovery.ExtraIPs)
	}
}

func TestLoadS3EndpointAlsoSetsPublicEndpoint(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "minio.internal:9000")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with S3 endpoint failed: %v", err)
	}

	if cfg.Storage.Endpoint != "minio.internal:9000" {
		t.Fatalf("endpoint = %q", cfg.Storage.Endpoint)
	}
	if cfg.Storage.PublicEndpoint != "minio.internal:9000" {
		t.Fatalf("public endpoint = %q, want S3 endpoint fallback", cfg.Storage.PublicEndpoint)
	}
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apiserver.yaml")
	content := []byte(`
server:
  port: 18191
  mode: debug
database:
  dsn: host=postgres user=drop dbname=drop sslmode=disable
  max_open_conns: 11
grpc:
  addr: server:50051
  mtls_cert_file: /certs/client.crt
  mtls_key_file: /certs/client.key
  mtls_ca_file: /certs/ca.crt
storage:
  endpoint: minio:9000
  public_endpoint: localhost:9000
  bucket: custom-bucket
log:
  level: debug
  format: console
security:
  environment: production
  allow_insecure_transport: false
observability:
  metrics_enabled: true
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load yaml failed: %v", err)
	}
	if cfg.Server.Port != 18191 || cfg.Server.Mode != "debug" {
		t.Fatalf("server config = %+v", cfg.Server)
	}
	if cfg.Database.MaxOpenConns != 11 {
		t.Fatalf("max open conns = %d", cfg.Database.MaxOpenConns)
	}
	if cfg.Storage.Bucket != "custom-bucket" {
		t.Fatalf("bucket = %q", cfg.Storage.Bucket)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "console" {
		t.Fatalf("log config = %+v", cfg.Log)
	}
	if cfg.Security.Environment != "production" || cfg.Security.AllowInsecureTransport {
		t.Fatalf("security yaml config = %+v", cfg.Security)
	}
	if cfg.GRPC.MTLSCAFile != "/certs/ca.crt" {
		t.Fatalf("grpc mTLS yaml config = %+v", cfg.GRPC)
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: [bad\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatalf("Load invalid yaml should fail")
	}
}

func TestProductionRequiresTrustedTransportOrExplicitInsecure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prod.yaml")
	content := []byte(`
security:
  environment: production
  allow_insecure_transport: false
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatalf("production config without mTLS or explicit insecure should fail")
	}
}
