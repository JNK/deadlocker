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
	if err := run(); err != nil {
		log.Fatalf("deadlocker: %v", err)
	}
}

func run() error {
	var (
		addr      = flag.String("addr", "127.0.0.1:8899", "address to serve the UI on")
		casesDir  = flag.String("cases", "cases", "directory containing scenario YAML files")
		statePath = flag.String("state", "", "path to the bbolt state file (default: <cases>/../.deadlocker/state.db)")
		settle    = flag.Duration("settle", 400*time.Millisecond, "how long a statement may run before it is reported as blocked")
		prewarm   = flag.String("prewarm", "", "start this MySQL image at boot instead of on the first run (e.g. mysql:8.4)")
		keepStale = flag.Bool("keep-stale", false, "do not remove containers left behind by a previous run")
		noSeed    = flag.Bool("no-seed", false, "do not copy the built-in example scenarios into the case directory")
	)
	flag.Parse()

	absCases, err := filepath.Abs(*casesDir)
	if err != nil {
		return err
	}
	// The example scenarios ship inside the binary, so an empty or missing
	// directory is not an error: it gets populated. Existing files are never
	// overwritten.
	if !*noSeed {
		res, err := casedef.Seed(absCases)
		if err != nil {
			return err
		}
		if n := len(res.Written); n > 0 {
			log.Printf("wrote %d built-in scenario(s) into %s", n, absCases)
		}
	} else if _, err := os.Stat(absCases); err != nil {
		return fmt.Errorf("case directory %s: %w", absCases, err)
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

	if *prewarm != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()
			log.Printf("pre-warming %s…", *prewarm)
			if _, err := pool.Get(ctx, *prewarm, func(s string) { log.Printf("  %s", s) }); err != nil {
				log.Printf("pre-warm failed: %v", err)
				return
			}
			log.Printf("%s is ready", *prewarm)
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
