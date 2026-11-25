package consistency

import (
	"context"
	"strconv"
	"strings"
)

var redisRoots = []string{"/data", "/bitnami/redis", "/bitnami/valkey"}

// memoryReserve is what a Redis fork must leave of the host's available
// memory after its worst case.
const memoryReserve = 256 << 20

// redisPassword reads the password from the environment the common images
// use, or from --requirepass on the command line, which is how the
// official image is given one.
func redisPassword(app App) string {
	if pw := first(app.Env["REDIS_PASSWORD"], app.Env["VALKEY_PASSWORD"], app.Env["KEYDB_PASSWORD"]); pw != "" {
		return pw
	}
	return flag(app.Cmd, "--requirepass")
}

func redisPort(app App) int {
	if v, err := strconv.Atoi(flag(app.Cmd, "--port")); err == nil && v > 0 && v < 65536 {
		return v
	}
	return port(app.Env, 6379, "REDIS_PORT_NUMBER", "VALKEY_PORT_NUMBER")
}

// planRedis streams an RDB the way a replica receives one (redis-cli
// --rdb): Redis forks, and the child writes a point-in-time snapshot while
// the parent keeps serving. Every page the parent writes during the dump is
// copied, so in the worst case the fork doubles Redis's memory; without
// room for that the dump is skipped and the last save on disk, which Redis
// replaces by rename and so is always whole, is read as a file.
func (p *Planner) planRedis(ctx context.Context, app App, plan *Plan) ([]string, error) {
	pw := redisPassword(app)
	if err := plainSecret(pw); err != nil {
		plan.warn(WarnCredentials, "%s", err.Error())
		return nil, nil
	}
	cli, _, err := p.tool(ctx, app, "redis-cli", "valkey-cli", "keydb-cli")
	if err != nil {
		return nil, err
	}
	if cli == "" {
		plan.warn(WarnToolMissing, "redis-cli is not in the image")
		return nil, nil
	}
	base := []string{cli, "-h", "127.0.0.1", "-p", strconv.Itoa(redisPort(app)), "--no-auth-warning"}
	c := command{argv: append(append([]string{}, base...), "INFO", "memory")}
	if pw != "" {
		c.secret, c.mode = []byte(pw), secretAsRedisAuth
	}
	out, ok, err := p.probe(ctx, app, plan, c, pw)
	if err != nil || !ok {
		return nil, err
	}
	used, rss := infoField(out, "used_memory"), infoField(out, "used_memory_rss")
	if used < 0 {
		plan.warn(WarnProbe, "Redis did not report its memory: %s", mask(firstLine(out), []string{pw}))
		return nil, nil
	}
	avail, err := p.cfg.AvailableMemory()
	if err != nil {
		return nil, err
	}
	if avail-used < memoryReserve {
		plan.warn(WarnMemory, "Redis uses %d bytes and the host has %d available; a fork could need as much again", used, avail)
		return nil, nil
	}
	if lim := app.MemoryLimit; lim > 0 && rss >= 0 && lim-rss < used {
		plan.warn(WarnMemory, "Redis uses %d bytes of its %d byte limit; a fork could need %d more", rss, lim, used)
		return nil, nil
	}
	c.argv = append(append([]string{}, base...), "--rdb", "-")
	plan.Dump = &Dump{Name: "redis", File: "redis.rdb", Consistency: Consistent, Spec: c.spec(app)}
	return redisRoots, nil
}

func infoField(out []byte, name string) int64 {
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && k == name {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
				return n
			}
		}
	}
	return -1
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// RDBTrailerSize is how many bytes to cut from the end of a stored Redis
// dump. redis-cli --rdb - passes through the end mark of a diskless
// transfer, 40 hex characters after the RDB's own end (an EOF opcode and
// an 8-byte checksum). Redis ignores it in dump.rdb but reads it as a
// broken command when the RDB stands in for an append-only file. tail is
// the dump's last bytes, at least 49 of them.
func RDBTrailerSize(tail []byte) int {
	const mark, end = 40, 9
	if len(tail) < mark+end || tail[len(tail)-mark-end] != 0xFF {
		return 0
	}
	for _, c := range tail[len(tail)-mark:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return 0
		}
	}
	return mark
}
