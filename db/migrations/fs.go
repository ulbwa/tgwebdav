// Package migrations embeds the dbmate SQL migrations (db/migrations/*.sql).
// dbmate is the only schema authority — there is no runtime auto-migration; the
// schema changes only through these versioned SQL files. The embedded files live
// alongside this file so go:embed can reach them while the migrations stay out of
// the internal tree. The runner that applies them lives in the cmd wiring layer.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
