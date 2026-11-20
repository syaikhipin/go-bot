package plugin

const SettingsPath = "/v1/settings/plugins"
const InternalPath = "/internal/host/plugins"

type Status struct {
	Runtime   string        `json:"runtime"`
	Packages  []PackageInfo `json:"packages"`
	Instances []Instance    `json:"instances"`
	Warnings  []string      `json:"warnings,omitempty"`
	Error     string        `json:"error,omitempty"`
}

type View struct {
	Catalog  []CatalogEntry `json:"catalog"`
	Settings Settings       `json:"settings"`
	Status   Status         `json:"status"`
}
