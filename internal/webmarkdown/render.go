// Package webmarkdown runs DeepSeek Harness's pinned fetch renderer in an
// isolated Goja runtime. Rendering uses only an in-memory DOM and text.
package webmarkdown

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"

	"github.com/dop251/goja"
	"go-harness/web"
)

//go:embed render.js
var source string

var renderer = sync.OnceValues(func() (*goja.Program, error) {
	return goja.Compile("web-fetch-renderer.js", source, false)
})

func Render(ctx context.Context, result web.FetchResult, maxChars int) (text string, truncated bool, err error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	program, err := renderer()
	if err != nil {
		return "", false, err
	}
	vm := goja.New()
	// Domino expects browser globals at initialization. No timers or console
	// effects are needed to parse and render a static document.
	ignore := func(goja.FunctionCall) goja.Value { return goja.Undefined() }
	for _, name := range []string{"setTimeout", "clearTimeout", "setInterval", "clearInterval"} {
		if err := vm.Set(name, ignore); err != nil {
			return "", false, err
		}
	}
	if err := vm.Set("console", map[string]any{"log": ignore, "warn": ignore, "error": ignore}); err != nil {
		return "", false, err
	}
	stop := context.AfterFunc(ctx, func() { vm.Interrupt(ctx.Err()) })
	defer stop()
	if _, err := vm.RunProgram(program); err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", false, fmt.Errorf("initialize fetch renderer: %w", err)
	}
	render, ok := goja.AssertFunction(vm.Get("renderWebFetch"))
	if !ok {
		return "", false, errors.New("missing fetch renderer")
	}
	value, err := render(goja.Undefined(), vm.ToValue(map[string]any{
		"url": result.URL, "statusCode": result.StatusCode, "truncated": result.Truncated,
		"body": map[string]string{"kind": result.Body.Kind, "content": result.Body.Content},
	}), vm.ToValue(maxChars))
	if ctx.Err() != nil {
		return "", false, ctx.Err()
	}
	if err != nil {
		return "", false, err
	}
	output := value.ToObject(vm)
	return output.Get("text").String(), output.Get("truncated").ToBoolean(), nil
}
