package consistency

import (
	"strings"
)

// imageEngines names the engine behind well-known image repositories.
var imageEngines = map[string]Engine{
	"postgres":                          Postgres,
	"postgis/postgis":                   Postgres,
	"timescale/timescaledb":             Postgres,
	"timescale/timescaledb-ha":          Postgres,
	"pgvector/pgvector":                 Postgres,
	"ankane/pgvector":                   Postgres,
	"bitnami/postgresql":                Postgres,
	"supabase/postgres":                 Postgres,
	"paradedb/paradedb":                 Postgres,
	"mysql":                             MySQL,
	"mysql/mysql-server":                MySQL,
	"bitnami/mysql":                     MySQL,
	"percona":                           MySQL,
	"percona/percona-server":            MySQL,
	"mariadb":                           MariaDB,
	"bitnami/mariadb":                   MariaDB,
	"linuxserver/mariadb":               MariaDB,
	"yobasystems/alpine-mariadb":        MariaDB,
	"redis":                             Redis,
	"redis/redis-stack-server":          Redis,
	"redis/redis-stack":                 Redis,
	"bitnami/redis":                     Redis,
	"valkey/valkey":                     Redis,
	"bitnami/valkey":                    Redis,
	"eqalpha/keydb":                     Redis,
	"mongo":                             Mongo,
	"bitnami/mongodb":                   Mongo,
	"mongodb/mongodb-community-server":  Mongo,
	"mongodb/mongodb-enterprise-server": Mongo,
	"clickhouse/clickhouse-server":      ClickHouse,
	"clickhouse":                        ClickHouse,
	"yandex/clickhouse-server":          ClickHouse,
	"bitnami/clickhouse":                ClickHouse,
}

// lastNameEngines catches mirrors and forks by the repository's last path
// component ("registry.example.com/team/postgres").
var lastNameEngines = map[string]Engine{
	"postgres":           Postgres,
	"postgresql":         Postgres,
	"postgis":            Postgres,
	"timescaledb":        Postgres,
	"pgvector":           Postgres,
	"mysql":              MySQL,
	"mysql-server":       MySQL,
	"percona-server":     MySQL,
	"mariadb":            MariaDB,
	"redis":              Redis,
	"redis-stack":        Redis,
	"redis-stack-server": Redis,
	"valkey":             Redis,
	"keydb":              Redis,
	"mongo":              Mongo,
	"mongodb":            Mongo,
	"clickhouse":         ClickHouse,
	"clickhouse-server":  ClickHouse,
}

// Repository is an image reference's repository path without registry,
// tag or digest, and without Docker Hub's "library/".
func Repository(image string) string {
	ref := strings.ToLower(strings.TrimSpace(image))
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		ref = ref[:i]
	}
	parts := strings.Split(ref, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		parts = parts[1:]
	}
	if len(parts) > 1 && parts[0] == "library" {
		parts = parts[1:]
	}
	return strings.Join(parts, "/")
}

// EngineOfImage names the engine an image reference shows, "" for none.
func EngineOfImage(image string) Engine {
	repo := Repository(image)
	if repo == "" || strings.HasPrefix(repo, "sha256") {
		return ""
	}
	if e, ok := imageEngines[repo]; ok {
		return e
	}
	return lastNameEngines[repo[strings.LastIndexByte(repo, '/')+1:]]
}

// engineFor picks the engine: the recipe's declaration wins over the
// image name, and a hook over both.
func (p *Planner) engineFor(app App) (Engine, *Hook) {
	if s := app.Spec; s != nil {
		if s.Hook != nil {
			return "", s.Hook
		}
		if s.Engine != "" {
			return s.Engine, nil
		}
	}
	return EngineOfImage(app.Image), nil
}
