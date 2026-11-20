package skills

import "path/filepath"

// CatalogSkill is the invocation-neutral public Skills service projection.
// Locators and provider functions stay in their owner process.
type CatalogSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	WhenToUse   string `json:"whenToUse,omitempty"`
	Invocation  struct {
		ModelInvocable bool `json:"modelInvocable"`
		UserInvocable  bool `json:"userInvocable"`
	} `json:"invocation"`
	Source       string        `json:"source"`
	Provider     string        `json:"provider"`
	Path         string        `json:"path,omitempty"`
	ResourceBase *ResourceBase `json:"resourceBase,omitempty"`
}

type CatalogDefinition struct {
	CatalogSkill
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (c Candidate) Catalog() CatalogSkill {
	var value CatalogSkill
	if c.presentation != nil {
		value = *c.presentation
	} else if c.loader != nil {
		value.Source, value.Provider = "custom", c.providerName
	} else {
		value.Provider = "filesystem"
		value.Path = filepath.Join(c.root, c.path)
		value.ResourceBase = &ResourceBase{Kind: "directory", Path: filepath.Join(c.root, c.base)}
		switch c.source.rank {
		case 100:
			value.Source = "project-dsh"
		case 200:
			value.Source = "project-agents"
		case 400:
			value.Source = "user-dsh"
		case 500:
			value.Source = "user-agents"
		default:
			value.Source = "custom"
		}
	}
	value.Name, value.Description, value.WhenToUse = c.Name, c.Description, c.WhenToUse
	value.Invocation.ModelInvocable, value.Invocation.UserInvocable = c.ModelInvocable, c.UserInvocable
	return value
}

func (c Candidate) Definition(loaded Loaded) CatalogDefinition {
	value := c.Catalog()
	value.Name, value.Description, value.WhenToUse = loaded.Name, loaded.Description, loaded.WhenToUse
	value.Invocation.ModelInvocable, value.Invocation.UserInvocable = loaded.ModelInvocable, loaded.UserInvocable
	value.Provider, value.ResourceBase = loaded.Value.Provider, loaded.Value.ResourceBase
	return CatalogDefinition{CatalogSkill: value, Content: loaded.Value.Content, Metadata: loaded.Metadata}
}
