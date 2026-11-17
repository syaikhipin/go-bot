package host

import (
	"context"
	"errors"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"

	"go-bot/internal/config"
	webproviders "go-bot/providers/web"
	builtin "go-bot/providers/web/builtin"
	"go-bot/web"
)

// This test doubles as a Host entrypoint in the Docker integration fixture.
// Production Run installs the same adapters with their normal network defaults.
func TestDockerWebFixtureHost(t *testing.T) {
	fixture := os.Getenv("HARNESS_TEST_WEB_FIXTURE")
	if fixture == "" {
		t.Skip("only run inside the Docker web parity fixture image")
	}
	c, err := config.LoadHost()
	if err != nil {
		t.Fatal(err)
	}
	search := func(options web.Options) (web.SearchProvider, error) {
		options.BaseURL = fixture + "/v1/tools"
		return webproviders.New("deepseek", options)
	}
	endpoint := strings.TrimPrefix(fixture, "http://")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, c, search, builtin.Fetcher{Dialer: fixtureDialer(endpoint)}); err != nil {
		t.Fatal(err)
	}
}

type fixtureDialer string

func (d fixtureDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if address != "8.8.8.8:80" {
		return nil, errors.New("unexpected Docker fetch destination")
	}
	return (&net.Dialer{}).DialContext(ctx, network, string(d))
}
