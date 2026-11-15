// Package webproviders registers the search adapters bundled with the Host.
package webproviders

import (
	"errors"
	"sort"

	"go-harness/internal/providersettings"
	"go-harness/providers/web/deepseek"
	"go-harness/providers/web/doubao"
	"go-harness/providers/web/zhipu"
	"go-harness/web"
)

type adapter struct {
	name        string
	newProvider web.Factory
}

var registry = map[string]adapter{
	deepseek.ID: {name: "DeepSeek", newProvider: deepseek.New},
	doubao.ID:   {name: "Doubao Search", newProvider: doubao.New},
	zhipu.ID:    {name: "Zhipu Search", newProvider: zhipu.New},
}

const Default = deepseek.ID

func Catalog() []providersettings.Descriptor {
	result := make([]providersettings.Descriptor, 0, len(registry))
	for id, adapter := range registry {
		result = append(result, providersettings.Descriptor{ID: id, Name: adapter.name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func Lookup(id string) (web.Factory, bool) {
	adapter, ok := registry[id]
	return adapter.newProvider, ok
}

func New(id string, options web.Options) (web.SearchProvider, error) {
	factory, ok := Lookup(id)
	if !ok {
		return nil, errors.New("unsupported WEB_SEARCH_PROVIDER")
	}
	return factory(options)
}
