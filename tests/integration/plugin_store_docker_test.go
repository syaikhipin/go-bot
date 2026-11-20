package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go-bot/internal/controller"
	"go-bot/plugin"
	dockerapi "go-bot/thirdparty/docker"
)

func TestDockerPluginSettingsStayInTheUserHash(t *testing.T) {
	if os.Getenv("RUN_DOCKER_INTEGRATION") != "1" {
		t.Skip("requires Docker and the Redis image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	docker := dockerapi.Client{}
	output, err := docker.Run(ctx, nil, "run", "--rm", "-d", "-p", "127.0.0.1::6379", "redis:8-alpine")
	if err != nil {
		t.Fatal(err)
	}
	container := strings.TrimSpace(string(output))
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = docker.Run(cleanup, nil, "rm", "-f", container)
	})
	output, err = docker.Run(ctx, nil, "port", container, "6379/tcp")
	if err != nil {
		t.Fatal(err)
	}
	address := strings.TrimSpace(string(output))
	var store *controller.RedisStore
	for store == nil {
		store, err = controller.OpenRedis(ctx, address)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer store.Close()
	if err = store.Create(ctx, "plugin-fixture", controller.Account{DesiredState: "stopped"}); err != nil {
		t.Fatal(err)
	}
	settings := plugin.Settings{Packages: map[string]plugin.PackageSelection{"@go-bot/plugin-officecli": {CatalogID: "officecli", Version: "0.1.0", Enabled: true}}}

	if err = store.SavePluginSettings(ctx, "plugin-fixture", settings); err != nil {
		t.Fatal(err)
	}
	// Host lifecycle writes must not clobber the independent settings field.
	if err = store.Save(ctx, "plugin-fixture", controller.Account{DesiredState: "running", HostID: "runtime-fixture"}); err != nil {
		t.Fatal(err)
	}
	record, err := store.Get(ctx, "plugin-fixture")
	if err != nil || record.Plugins == nil || len(record.Plugins.Packages) != 1 || !record.Plugins.Packages["@go-bot/plugin-officecli"].Enabled {
		t.Fatalf("account plugin data did not survive lifecycle save: %v", err)
	}
	output, err = docker.Run(ctx, nil, "exec", container, "redis-cli", "--raw", "KEYS", "*")
	if err != nil || strings.TrimSpace(string(output)) != "user:plugin-fixture" {
		t.Fatalf("plugin settings created another Redis key: %q %v", output, err)
	}
	output, err = docker.Run(ctx, nil, "exec", container, "redis-cli", "--raw", "TYPE", "user:plugin-fixture")
	if err != nil || strings.TrimSpace(string(output)) != "hash" {
		t.Fatalf("account is not a Hash: %q %v", output, err)
	}
	output, err = docker.Run(ctx, nil, "exec", container, "redis-cli", "--raw", "HGET", "user:plugin-fixture", "plugins")
	var actual plugin.Settings
	if err != nil || json.Unmarshal(output, &actual) != nil || actual.Packages["@go-bot/plugin-officecli"].Version != "0.1.0" || strings.Contains(string(output), "instances") || strings.Contains(string(output), "source") {
		t.Fatal("saved plugin field is not the expected settings JSON")
	}
	if err = store.SavePluginSettings(ctx, "absent", settings); !errors.Is(err, controller.ErrAccountNotFound) {
		t.Fatalf("missing account write: %v", err)
	}
}
