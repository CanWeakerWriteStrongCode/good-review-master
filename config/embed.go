package config

import _ "embed"

//go:embed config_example.yaml
var configExampleTemplate []byte

//go:embed prompt_system_example.yaml
var promptSystemExampleTemplate []byte
