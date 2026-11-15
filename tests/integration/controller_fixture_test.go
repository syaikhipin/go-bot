package integration

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go-harness/internal/config"
	"go-harness/internal/controller"
	"go-harness/internal/modelsettings"
	dock "go-harness/providers/runtime/docker"
	"go-harness/runtime"
	dockerapi "go-harness/thirdparty/docker"
)

type fixtureManager struct {
	*controller.Manager
	driver  *dock.Driver
	account string
}

func dockerFixtureConfig() config.Config {
	c := config.Default()
	if image := os.Getenv("HARNESS_TEST_HOST_IMAGE"); image != "" {
		c.Runtime.Image = image
	}
	return c
}

func newFixtureManager(t *testing.T, c config.Config) *fixtureManager {
	t.Helper()
	return newFixtureManagerForAccount(t, c, "fixture-"+uuid.NewString())
}

func newFixtureManagerForAccount(t *testing.T, c config.Config, account string) *fixtureManager {
	t.Helper()
	client := dockerapi.Client{}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	output, err := client.Run(ctx, nil, "run", "--rm", "-d", "-p", "127.0.0.1::6379", "redis:8-alpine")
	if err != nil {
		t.Fatal(err)
	}
	container := strings.TrimSpace(string(output))
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		_, _ = client.Run(cleanup, nil, "rm", "-f", container)
	})
	output, err = client.Run(ctx, nil, "port", container, "6379/tcp")
	if err != nil {
		t.Fatal(err)
	}
	var store *controller.RedisStore
	for store == nil {
		store, err = controller.OpenRedis(ctx, strings.TrimSpace(string(output)))
		if ctx.Err() != nil {
			t.Fatalf("Redis did not become ready: %v", err)
		}
		if store == nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	t.Cleanup(func() { _ = store.Close() })
	driver, err := controller.Driver(c)
	if err != nil {
		t.Fatal(err)
	}
	manager := controller.New(c, driver, store)
	dockerDriver, isDocker := driver.(*dock.Driver)
	if isDocker {
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
			defer done()
			if _, err := client.Run(cleanup, nil, "volume", "rm", "gh-"+account+"-data"); err != nil && !strings.Contains(err.Error(), "no such volume") {
				t.Errorf("remove fixture volume: %v", err)
			}
		})
	}
	model := c.Model.Main
	if err := manager.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveModelCatalog(ctx, account, modelsettings.Catalog{
		DefaultEndpointID: "fixture",
		Endpoints:         map[string]modelsettings.Endpoint{"fixture": {Name: "Fixture", BaseURL: model.BaseURL, APIKey: model.APIKey, Models: []string{model.Name}}},
	}); err != nil {
		t.Fatal(err)
	}
	return &fixtureManager{Manager: manager, driver: dockerDriver, account: account}
}

func (m *fixtureManager) Ensure(ctx context.Context) error {
	return m.Manager.Ensure(ctx, m.account)
}
func (m *fixtureManager) Stop(ctx context.Context) error {
	return m.Manager.Stop(ctx, m.account)
}
func (m *fixtureManager) Delete(ctx context.Context) error {
	return m.Manager.Delete(ctx, m.account)
}
func (m *fixtureManager) Instance(ctx context.Context) *runtime.Instance {
	status, err := m.Observe(ctx, m.account)
	if err != nil || status.HostID == "" {
		return nil
	}
	instance, err := m.driver.Find(ctx, runtime.Spec{RunID: status.HostID})
	if err != nil {
		return nil
	}
	return &instance
}
func (m *fixtureManager) Exec(t *testing.T, ctx context.Context, args ...string) []byte {
	t.Helper()
	instance := m.Instance(ctx)
	if instance == nil {
		t.Fatal("Host instance missing")
	}
	out, err := (dockerapi.Client{}).Run(ctx, nil, append([]string{"exec", instance.Ref.ID}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func (m *fixtureManager) Journal(t *testing.T, ctx context.Context, sessionID string) []byte {
	t.Helper()
	return m.Exec(t, ctx, "sh", "-ec", `set -- /root/harness/.dsh/sessions/*/"$1"/session.v4.jsonl; test "$#" -eq 1; cat "$1"`, "sh", sessionID)
}
func (m *fixtureManager) Handler() http.Handler {
	return m.HandlerWithWeb(controller.WebOptions{})
}
func (m *fixtureManager) HandlerWithWeb(options controller.WebOptions) http.Handler {
	handler := m.Manager.HandlerWithWeb(options)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(controller.UserHeader, m.account)
		handler.ServeHTTP(w, r)
	})
}
