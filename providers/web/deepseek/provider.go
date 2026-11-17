package deepseek

import (
	"context"
	"net/http"
	"sync"
	"time"

	api "go-bot/thirdparty/deepseek"
	"go-bot/web"
)

const ID = "deepseek"

type Provider struct {
	Client api.Client
	mu     sync.Mutex
	starts []time.Time
}

func New(o web.Options) (web.SearchProvider, error) {
	if o.BaseURL == "" {
		o.BaseURL = "https://api.deepseek.com/v1/tools"
	}
	return &Provider{Client: api.Client{BaseURL: o.BaseURL, APIKey: o.APIKey, HTTP: &http.Client{Timeout: 60 * time.Second}}}, nil
}
func (*Provider) Name() string { return ID }

func (p *Provider) Search(ctx context.Context, r web.SearchRequest) (web.SearchResult, error) {
	count := 10
	if r.MaxResults > 0 {
		count = min(count, r.MaxResults)
	}
	question := []rune(web.CollapseSpace(r.OriginalQuestion))
	if len(question) > 128 {
		question = question[:128]
	}
	if err := p.waitForSlot(ctx); err != nil {
		return web.SearchResult{}, &web.Error{Code: "WEB_ABORTED", Message: "DeepSeek web request aborted", Cause: err}
	}
	return p.Client.Search(ctx, r.Query, count, string(question))
}

func (p *Provider) waitForSlot(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		now := time.Now()
		for len(p.starts) > 0 && now.Sub(p.starts[0]) > time.Second {
			p.starts = p.starts[1:]
		}
		if len(p.starts) < 5 {
			p.starts = append(p.starts, now)
			p.mu.Unlock()
			return nil
		}
		wait := time.Until(p.starts[0].Add(time.Second + time.Millisecond))
		p.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
