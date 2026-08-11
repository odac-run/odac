// odac-server is the Go orchestrator (Phase 3 of the Node→Go migration; the
// counterpart of node server/index.js). Until task 3.8 flips the cutover it
// is built and tested but never wired into bin/odac or the Docker image —
// the Node orchestrator keeps running production.
//
// Lifecycle notes (parity with Node, see contracts/lifecycle.md):
//   - No signal handlers, like Node: SIGTERM/SIGINT kill the process; the
//     watchdog treats any non-zero exit as a crash and restarts. Config loss
//     is bounded by the 500ms autosave — Node has no exit-time save either
//     (the update handover exits via process.exit(0) mid-interval too).
//   - No panic recovery: Node's uncaughtException handler logs and exits 1.
//     A Go panic prints its stack and exits 2 — same watchdog outcome.
//     Node's global ECONNRESET/EPIPE swallowing needs no equivalent: those
//     surface as ordinary error returns at their call sites in Go.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"odac/internal/api"
	"odac/internal/appmgr"
	"odac/internal/config"
	"odac/internal/dataplane"
	"odac/internal/docker"
	"odac/internal/domains"
	"odac/internal/hub"
	"odac/internal/logx"
	"odac/internal/sysinfo"
	"odac/internal/system"
	"odac/internal/system/swap"
	"odac/internal/updater"
)

func main() {
	log := logx.New("Server")

	baseDir := config.DefaultBaseDir()
	cfg, err := config.Open(baseDir)
	if err != nil {
		log.Error("Config initialization failed:", err.Error())
		os.Exit(1)
	}

	// Server-side autosave per contract 0.6; the stop function is unused —
	// autosave runs for the life of the process, exactly like Node's
	// unref'd 500ms interval.
	cfg.AutoSave(500*time.Millisecond, func(err error) {
		log.Error("Config autosave failed:", err.Error())
	})

	// Docker client (Container.js port). Availability is probed once here,
	// exactly like Node's constructor-time ping; a docker-less host keeps a
	// non-nil but unavailable client and every operation no-ops.
	containers, err := docker.Connect(docker.Options{
		HostRoot: os.Getenv("ODAC_HOST_ROOT"),
		LogsRoot: filepath.Join(baseDir, "logs"),
	})
	if err != nil {
		// Client construction only fails on malformed DOCKER_HOST-style env;
		// treat it like an unreachable daemon (Node would too).
		log.Error("Docker client initialization failed:", err.Error())
	}

	binDir := moduleBinDir()
	dnsSvc := dataplane.NewDNS(cfg, binDir)
	var containerIPs dataplane.ContainerIPs
	if containers != nil {
		containerIPs = containers
	}
	proxySvc := dataplane.NewProxy(cfg, binDir, containerIPs)
	mailSvc := dataplane.NewMail(cfg, binDir, dnsSvc)

	apiSrv := api.NewServer(cfg)
	apiSrv.Addr = os.Getenv("ODAC_API_ADDR") // test/smoke override only, like the CLI's
	apiSrv.Init()                            // generates config.api.auth on first start

	// Domain + SSL managers (task 3.5). SSL rides the 1s check tick; Domain
	// fills appmgr's DomainDeleter seam for app-delete cascades.
	sslSvc := domains.NewSSL(cfg, dnsSvc, proxySvc, mailSvc)
	domainSvc := domains.NewDomain(cfg, dnsSvc, sslSvc, proxySvc, mailSvc)

	// System inventory. Built before appmgr because it also answers the GPU
	// pre-flight ("can this engine hand a card to a container"), which app
	// creation consults before pulling a single byte.
	sysInfo := sysinfo.New(
		func() bool { return containers != nil && containers.Available() },
		func() []string {
			if containers == nil {
				return nil
			}
			return containers.Runtimes()
		},
	)

	// App manager (task 3.4e). Skipped only when the Docker client could not
	// even be constructed (malformed DOCKER_HOST-style env) — an unreachable
	// daemon still yields a client, and appmgr no-ops like Node does.
	var appMgr *appmgr.Manager
	if containers != nil {
		appMgr = appmgr.New(cfg, filepath.Join(baseDir, "logs"), appmgr.Deps{
			Docker:  containers,
			Api:     apiSrv,
			Proxy:   proxySvc,
			Domains: domainSvc,
			GPUHost: sysInfo,
		})
		appMgr.Init() // Node: the DI registry runs App.init() on first resolve
	}

	// Updater (task 3.7): zero-downtime self-update. It owns its own SDK
	// client like Node's Updater constructs its own dockerode instance; a
	// docker-less host degrades to an always-failing client (update checks
	// answer like Node's would against a dead socket).
	updDocker, uerr := updater.ConnectDocker()
	if uerr != nil {
		log.Error("Updater docker client initialization failed:", uerr.Error())
	}
	upd := updater.New(baseDir, updater.Deps{
		Docker: updDocker,
		Proxy:  proxySvc,
		DNS:    dnsSvc,
	})

	// Hub client (task 3.6); app/container slots stay nil on docker-less
	// hosts. system.update delegates to the updater like System.update().
	hubURL := os.Getenv("ODAC_HUB_URL")
	if hubURL == "" {
		hubURL = hub.DefaultURL
	}
	hubDeps := hub.Deps{
		DNS:      dnsSvc,
		Domain:   domainSvc,
		Proxy:    proxySvc,
		Mail:     mailSvc,
		SysInfo:  sysInfo.Get,
		SysStats: sysInfo.Stats,
		SysUpdate: func() (any, error) {
			r, err := upd.Start()
			if err != nil {
				return nil, err
			}
			return r, nil
		},
	}
	if appMgr != nil {
		hubDeps.App = appMgr
	}
	if containers != nil {
		hubDeps.Container = hubContainers{containers}
		// Terminal sessions validate the target against the managed-app
		// list (Node went through App.list(); only the names are consulted).
		containers.SetAppNames(func() []string {
			var names []string
			cfg.View(func() {
				apps, _ := cfg.Get("apps").([]any)
				for _, item := range apps {
					if app, _ := item.(map[string]any); app != nil {
						if name, _ := app["name"].(string); name != "" {
							names = append(names, name)
						}
					}
				}
			})
			return names
		})
	}
	hubSvc := hub.New(cfg, hubURL, hubDeps)
	if appMgr != nil {
		appMgr.SetHub(hubSvc) // closes the 3.4e seam (triggers + recipe fetch)
	}
	mailSvc.SetHub(hubSvc)
	// A GPU that becomes schedulable mid-flight (driver loaded, toolkit
	// installed, daemon restarted) must not wait for the hourly system.info.
	sysInfo.SetGPUChangeHook(func() { hubSvc.Trigger("system.info") })

	svc := system.Services{
		Proxy: proxySvc,
		DNS:   dnsSvc,
		Mail:  mailSvc,
		Api:   apiSrv,
		SSL:   sslSvc,
		Hub:   hubSvc,
		Swap:  swap.New(cfg),
	}
	if appMgr != nil {
		svc.App = appMgr
	}
	sys := system.New(cfg, svc, upd)
	upd.SetSystem(sys) // closes the System↔Updater cycle (rollback re-Init)

	registerActions(apiSrv, sys, upd, dnsSvc, mailSvc, appMgr, domainSvc, sslSvc, hubSvc)

	if err := sys.Init(); err != nil {
		log.Error("System initialization failed:", err.Error())
		os.Exit(1)
	}
	log.Log("Odac server started")

	select {} // run until the watchdog (or a signal) kills us
}

// hubContainers adapts *docker.Client to hub.ContainerService — Go cannot
// covary CreateTerminalSession's concrete *docker.Terminal return into the
// interface's hub.TerminalExec.
type hubContainers struct{ *docker.Client }

func (h hubContainers) CreateTerminalSession(appName string, opts docker.TerminalOptions) (hub.TerminalExec, error) {
	t, err := h.Client.CreateTerminalSession(appName, opts)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// registerActions wires the full contract-0.1 action table (complete as of
// task 3.7 — every action in Node's Api.js #commands is registered).
func registerActions(apiSrv *api.Server, sys *system.System, upd *updater.Updater, dnsSvc *dataplane.DNS, mailSvc *dataplane.Mail, appMgr *appmgr.Manager, domainSvc *domains.Domain, sslSvc *domains.SSL, hubSvc *hub.Hub) {
	res := func(r api.Result) (*api.Result, error) { return &r, nil }

	apiSrv.Register("auth", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(hubSvc.Auth(a.At(0)))
	})
	// Node: update → System.update() → Updater.start(). A returned error
	// renders as result(false, message), matching Node's thrown path.
	apiSrv.Register("update", func(_ api.Args, _ api.Progress) (*api.Result, error) {
		r, err := upd.Start()
		if err != nil {
			return nil, err
		}
		return &r, nil
	})

	if appMgr != nil {
		// argStr renders an argument the way Node's string coercions saw it,
		// with JS null/undefined staying empty (falsy) rather than "null".
		argStr := func(v any) string {
			if v == nil {
				return ""
			}
			if s, ok := v.(string); ok {
				return s
			}
			return fmt.Sprint(v)
		}

		apiSrv.Register("app.create", func(a api.Args, _ api.Progress) (*api.Result, error) {
			return appMgr.Create(a.At(0)), nil
		})
		apiSrv.Register("app.delete", func(a api.Args, _ api.Progress) (*api.Result, error) {
			purge := true // Node: delete(id, {purge = true} = {})
			if opts, ok := a.At(1).(map[string]any); ok {
				if v, present := opts["purge"]; present {
					purge = v == true
				}
			}
			return appMgr.Delete(a.At(0), purge), nil
		})
		apiSrv.Register("app.device.add", func(a api.Args, _ api.Progress) (*api.Result, error) {
			return appMgr.DeviceAdd(a.At(0), argStr(a.At(1)), argStr(a.At(2))), nil
		})
		apiSrv.Register("app.device.delete", func(a api.Args, _ api.Progress) (*api.Result, error) {
			return appMgr.DeviceDelete(a.At(0), argStr(a.At(1))), nil
		})
		apiSrv.Register("app.list", func(a api.Args, _ api.Progress) (*api.Result, error) {
			return appMgr.List(a.At(0) == true), nil
		})
		apiSrv.Register("app.privileged", func(a api.Args, _ api.Progress) (*api.Result, error) {
			return appMgr.SetPrivileged(a.At(0), argStr(a.At(1))), nil
		})
		apiSrv.Register("app.restart", func(a api.Args, _ api.Progress) (*api.Result, error) {
			return appMgr.Restart(a.At(0)), nil
		})
		apiSrv.Register("app.start", func(a api.Args, _ api.Progress) (*api.Result, error) {
			return appMgr.Start(argStr(a.At(0))), nil
		})
	}

	apiSrv.Register("dns.list", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(dnsSvc.List(a.At(0)))
	})
	apiSrv.Register("domain.add", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(domainSvc.Add(a.At(0), a.At(1)))
	})
	apiSrv.Register("domain.delete", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(domainSvc.Delete(a.At(0), a.At(1) == true))
	})
	apiSrv.Register("domain.list", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(domainSvc.List(a.At(0)))
	})
	apiSrv.Register("ssl.renew", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(sslSvc.Renew(a.At(0)))
	})
	apiSrv.Register("mail.create", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(mailSvc.Create(a.At(0), a.At(1), a.At(2)))
	})
	apiSrv.Register("mail.delete", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(mailSvc.Delete(a.At(0)))
	})
	apiSrv.Register("mail.list", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(mailSvc.List(a.At(0)))
	})
	apiSrv.Register("mail.password", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(mailSvc.Password(a.At(0), a.At(1), a.At(2)))
	})
	apiSrv.Register("mail.send", func(a api.Args, _ api.Progress) (*api.Result, error) {
		return res(mailSvc.Send(a.At(0), a.Raw(0)))
	})
	// System.stop() returns nothing; the nil Result reproduces Node's
	// {"id":"..."}-only final response for server.stop.
	apiSrv.Register("server.stop", func(_ api.Args, _ api.Progress) (*api.Result, error) {
		sys.Stop(false)
		return nil, nil
	})
}

// moduleBinDir locates the data-plane binaries (odac-proxy/dns/mail). They
// ship next to this executable in bin/ (Node resolved the same directory as
// ../../bin relative to server/src). ODAC_BIN_DIR overrides for tests only.
func moduleBinDir() string {
	if dir := os.Getenv("ODAC_BIN_DIR"); dir != "" {
		return dir
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}
