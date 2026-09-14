package tf

import _ "embed"

//go:embed templates/state.go.tmpl
var stateTemplate string

//go:embed templates/resource.go.tmpl
var resourceTemplate string
