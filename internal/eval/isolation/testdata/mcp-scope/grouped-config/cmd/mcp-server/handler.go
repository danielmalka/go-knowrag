// A configuration whose scope sits one level down. This is the false positive the earlier version
// of this case produced: it required the scope's value to be a single selector on a plain
// identifier, so `cfg.Defaults.TenantID` failed it even though the value comes wholly from
// configuration and no tool input is anywhere near it.
//
// Nothing here may be reported. A case that fails a correct refactor teaches the maintainer to
// stop reading it. Not built or linted: testdata is ignored by the go tool.
package main

import "github.com/danielmalka/go-knowrag/internal/retrieval"

type toolInput struct {
	Query string `json:"query"`
	Area  string `json:"area,omitempty"`
}

type scopeDefaults struct {
	Collection string
	TenantID   string
}

type config struct {
	Defaults scopeDefaults
}

func search(cfg config, in toolInput) retrieval.Query {
	return retrieval.Query{
		Collection: cfg.Defaults.Collection,
		TenantID:   cfg.Defaults.TenantID,
		Text:       in.Query,
		Area:       in.Area,
	}
}
