// Command deadlocker serves the MySQL lock playground.
//
// It manages its own MySQL container, proxies every scenario connection through
// a decoding MySQL wire proxy, and serves a step-through UI on localhost.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jnk/deadlocker/internal/agentapi"
	"github.com/jnk/deadlocker/internal/casedef"
	"github.com/jnk/deadlocker/internal/chat"
	"github.com/jnk/deadlocker/internal/dockerctl"
	"github.com/jnk/deadlocker/internal/engine"
	"github.com/jnk/deadlocker/internal/mcpserver"
	"github.com/jnk/deadlocker/internal/mysqlbox"
	"github.com/jnk/deadlocker/internal/store"
	"github.com/jnk/deadlocker/internal/web"
)

func main() {
	// Subcommands are dispatched before flag.Parse so `run` can have its own
	// flag set; with no subcommand the tool serves the UI, which is what it is
	// for most of the time.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "run":
			fail(runCLI(os.Args[2:]))
			return
		case "help", "-h", "--help":
			fmt.Print(cliUsage)
			fmt.Println("\nServer flags:")
			flag.CommandLine.SetOutput(os.Stdout)
			flag.PrintDefaults()
			return
		}
	}
	fail(run())
}

// fail reports an error and exits, honouring an explicit exit code when one was
// asked for. A scenario disagreeing with reality is a result, not a crash, so it
// gets a plain message rather than a log line with a prefix.
func fail(err error) {
	if err == nil {
		return
	}
	var exit *exitError
	if errors.As(err, &exit) {
		fmt.Fprintln(os.Stderr, exit.msg)
		os.Exit(exit.code)
	}
	log.Fatalf("deadlocker: %v", err)
}

func run() error {
	var (
		addr      = flag.String("addr", "127.0.0.1:8899", "address to serve the UI on")
		casesDir  = flag.String("cases", "cases", "directory containing scenario YAML files")
		statePath = flag.String("state", "", "path to the bbolt state file (default: <cases>/../.deadlocker/state.db)")
		settle    = flag.Duration("settle", 400*time.Millisecond, "how long a statement may run before it is reported as blocked")
		prewarm   = flag.String("prewarm", "", "start this MySQL image at boot; overrides the setting in the UI")
		keepStale = flag.Bool("keep-stale", false, "do not remove containers left behind by a previous run")
		seed      = flag.Bool("seed", false, "copy the built-in example scenarios into the case directory at startup")
	)
	flag.Parse()

	absCases, err := filepath.Abs(*casesDir)
	if err != nil {
		return err
	}
	// The built-in scenarios ship inside the binary but are no longer written
	// out on startup. Filling someone's case directory with two dozen files
	// they did not ask for is a decision, not a default — the library page
	// offers the import while it is empty, and Settings offers it always.
	if err := os.MkdirAll(absCases, 0o755); err != nil {
		return fmt.Errorf("case directory %s: %w", absCases, err)
	}
	if *seed {
		res, err := casedef.Seed(absCases)
		if err != nil {
			return err
		}
		log.Printf("wrote %d built-in scenario(s) into %s", len(res.Written), absCases)
	}

	dbPath := *statePath
	if dbPath == "" {
		dbPath = filepath.Join(filepath.Dir(absCases), ".deadlocker", "state.db")
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Printf("configuration stored in %s", dbPath)

	if err := mysqlbox.SilenceExpectedDriverNoise(); err != nil {
		return err
	}

	docker, err := dockerctl.New()
	if err != nil {
		return err
	}
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 10*time.Second)
	err = docker.Ping(pingCtx)
	cancelPing()
	if err != nil {
		return fmt.Errorf("cannot reach the Docker daemon: %w", err)
	}

	lib := casedef.NewLibrary(absCases)
	if err := lib.Load(); err != nil {
		return fmt.Errorf("load cases: %w", err)
	}
	log.Printf("loaded %d scenario(s) from %s", len(lib.List()), absCases)
	for path, problem := range lib.Broken() {
		log.Printf("skipping %s: %s", path, problem)
	}

	// The manager is created before the pool so container logs can be routed
	// into the runs that are watching them.
	var mgr *engine.Manager
	pool := mysqlbox.NewPool(docker, func(image string, line dockerctl.LogLine) {
		if mgr != nil {
			mgr.OnDockerLog(image, line)
		}
	})
	mgr = engine.NewManager(pool, *settle)

	if !*keepStale {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if n, err := pool.ReapStale(ctx); err != nil {
			log.Printf("could not check for stale containers: %v", err)
		} else if n > 0 {
			log.Printf("removed %d stale container(s) from a previous session", n)
		}
		cancel()
	}

	// One operation layer, shared by the MCP endpoint and the built-in
	// assistant so the two can never drift apart.
	hub := agentapi.NewHub()
	api := agentapi.New(lib, mgr, hub)

	// Scenario history is recorded from here on, and the scenarios already on
	// disk get a baseline revision so there is always something to roll back to.
	api.UseVersions(st)
	if n := api.SeedVersions("as found on disk"); n > 0 {
		log.Printf("recorded a baseline version for %d scenario(s)", n)
	}

	assistant := chat.NewService(api, st)

	srv, err := web.NewServer(web.Deps{
		Library: lib,
		Manager: mgr,
		Pool:    pool,
		API:     api,
		Chat:    assistant,
		Store:   st,
		MCP:     mcpserver.Handler(api),
	})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Deliberately no WriteTimeout: the SSE endpoint streams for the whole
		// life of a run.
		IdleTimeout: 120 * time.Second,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	// Prewarming is a setting first and a flag second: the flag is for scripts,
	// the setting is for the person who does not want to wait for a pull every
	// morning. Either way it runs in its own goroutine — the UI has to come up
	// now, not in two minutes.
	image := *prewarm
	if image == "" {
		if cfg, _, err := st.Current(); err == nil && cfg.MySQL.Prewarm {
			image = cfg.MySQL.Image()
		}
	}
	if image != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()
			log.Printf("pre-warming %s…", image)
			if _, err := pool.Get(ctx, image, func(s string) { log.Printf("  %s", s) }); err != nil {
				// Not fatal: every run starts its own container on demand, so a
				// failed pre-warm costs time on the first run and nothing else.
				log.Printf("pre-warm failed, will start on demand instead: %v", err)
				return
			}
			log.Printf("%s is ready", image)
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("Deadlocker is listening on http://%s (MCP at http://%s/mcp)", ln.Addr(), ln.Addr())
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Printf("got %s, shutting down", sig)
	}

	// Release the event streams first; otherwise Shutdown waits out its whole
	// timeout for connections that are meant to stay open.
	srv.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}

	log.Printf("closing runs…")
	mgr.CloseAll()
	log.Printf("removing containers…")
	if err := pool.Close(); err != nil {
		log.Printf("container cleanup: %v", err)
	}
	log.Printf("done")
	return nil
}
