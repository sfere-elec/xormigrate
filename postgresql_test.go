// +build pgx

package xormigrate

import (
	_ "github.com/jackc/pgx/v4/stdlib"
)

func init() {
	databases = append(databases, database{
		name:    "pgx",
		connEnv: "PG_CONN_STRING",
	})
}
