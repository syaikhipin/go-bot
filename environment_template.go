package gobot

import _ "embed"

// EnvironmentTemplate is the public deployment template used to initialize private profiles.
//
//go:embed .env.example
var EnvironmentTemplate string
