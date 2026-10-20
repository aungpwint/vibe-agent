// Package migrations embeds the vibe-agent schema files. They live in this
// module, never in the ThamBandha API's migration chain: the tool owns a
// database of its own and applies these files itself.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
