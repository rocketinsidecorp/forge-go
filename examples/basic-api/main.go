// Command basic-api is the reference Forge service: a small task API backed
// by PostgreSQL and laid out as ports and adapters (see docs/adr/0004).
//
//	basic-api            serve HTTP (default)
//	basic-api migrate    apply the embedded migrations
//	basic-api rollback   revert the latest migration
package main

import (
	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/bootstrap"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/db"
	"github.com/rocketinsidecorp/forge-go/postgres"
)

func main() { forge.Main(bootstrap.Configure, postgres.Commands(db.Migrations())...) }
