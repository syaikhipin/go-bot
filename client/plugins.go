package client

import (
	"context"
	"net/http"
	"net/url"

	"go-bot/plugin"
)

type PluginPackageInput struct {
	PluginID string `json:"pluginId"`
}

func (c Client) GetPluginSettings(ctx context.Context) (view plugin.View, err error) {
	err = c.readJSON(ctx, http.MethodGet, plugin.SettingsPath, nil, &view)
	return
}
func (c Client) InstallPluginPackage(ctx context.Context, input PluginPackageInput) (view plugin.View, err error) {
	err = c.readJSON(ctx, http.MethodPost, plugin.SettingsPath+"/packages", input, &view)
	return
}
func (c Client) UpdatePluginPackage(ctx context.Context, name string, input PluginPackageInput) (view plugin.View, err error) {
	err = c.readJSON(ctx, http.MethodPost, plugin.SettingsPath+"/packages/"+url.PathEscape(name)+"/update", input, &view)
	return
}
func (c Client) RemovePluginPackage(ctx context.Context, name string) (view plugin.View, err error) {
	err = c.readJSON(ctx, http.MethodDelete, plugin.SettingsPath+"/packages/"+url.PathEscape(name), nil, &view)
	return
}
func (c Client) SetPluginEnabled(ctx context.Context, name string, enabled bool) (view plugin.View, err error) {
	err = c.readJSON(ctx, http.MethodPut, plugin.SettingsPath+"/packages/"+url.PathEscape(name), plugin.PackageInput{Enabled: enabled}, &view)
	return
}
func (c Client) RestartPlugins(ctx context.Context) (view plugin.View, err error) {
	err = c.readJSON(ctx, http.MethodPost, "/v1/host/plugins/runtime/restart", nil, &view)
	return
}
