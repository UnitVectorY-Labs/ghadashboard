package main

import "testing"

func TestConfiguration(t *testing.T) {
	for _, key := range []string{"GHA_DASHBOARD_MODE", "DATABASE_URL", "PUBSUB_PROJECT_ID", "PUBSUB_SUBSCRIPTION_ID", "HTTP_ADDR"} {
		t.Setenv(key, "")
	}
	if _, err := loadConfig(); err == nil {
		t.Fatal("missing database accepted")
	}
	t.Setenv("DATABASE_URL", "postgres://test")
	c, err := loadConfig()
	if err != nil || c.Mode != "web" || c.Address != ":8080" {
		t.Fatalf("web defaults: %+v %v", c, err)
	}
	t.Setenv("GHA_DASHBOARD_MODE", "consumer")
	if _, err := loadConfig(); err == nil {
		t.Fatal("consumer accepted without subscription")
	}
	t.Setenv("PUBSUB_PROJECT_ID", "test")
	t.Setenv("PUBSUB_SUBSCRIPTION_ID", "test-sub")
	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHA_DASHBOARD_MODE", "invalid")
	if _, err := loadConfig(); err == nil {
		t.Fatal("invalid mode accepted")
	}
}
