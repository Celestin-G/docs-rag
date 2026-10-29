package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
	"github.com/celestingeniaut/docs-rag/internal/server"
	"github.com/celestingeniaut/docs-rag/internal/store"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the web UI on http://localhost:8080",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().String("addr", ":8080", "Address to listen on")
	rootCmd.AddCommand(serveCmd)
}

// defaultShutdownTimeout bounds how long a graceful shutdown waits for
// in-flight requests to finish before the listener is forced closed.
// Overridable via SHUTDOWN_TIMEOUT for deployments that need to give slow
// clients more — or less — room.
//
// Sized so the whole worst-case exit fits in the 30s that container
// orchestrators commonly default terminationGracePeriodSeconds to (e.g.
// Kubernetes): SHUTDOWN_TIMEOUT (20s) + handlerGrace (5s) + the request-log
// drain, each write bounded by internal/server's logWriteTimeout (5s) =
// 30s. A longer default would not buy more drain time there: the
// orchestrator SIGKILLs the process once its own grace period expires,
// whatever SHUTDOWN_TIMEOUT says, which is worse than letting the forced
// path below run first (in-flight work cancelled, logs drained, DB pool
// closed cleanly).
//
// Deliberately not raised to cover ANSWER_TOTAL_BUDGET's full 120s ceiling:
// that is a safety ceiling for a slow model, not the typical answer time —
// most answers finish in a few seconds. An operator who raises
// ANSWER_TOTAL_BUDGET for a consistently slow backend should raise
// SHUTDOWN_TIMEOUT — and their orchestrator's termination grace period, to
// at least SHUTDOWN_TIMEOUT + 10s — to match; see the README's "Health and
// shutdown" section.
const defaultShutdownTimeout = 20 * time.Second

// handlerGrace bounds how long runServe waits for in-flight HTTP handlers to
// actually return after their context has been forcibly cancelled (see the
// DeadlineExceeded branch in runServe), before draining request-log writes
// and closing the database. Short on purpose: by this point Shutdown has
// already given every request its full SHUTDOWN_TIMEOUT to finish gracefully
// and failed, so a handler that respects its context should unblock almost
// immediately once that context is cancelled — this only guards against one
// that doesn't.
const handlerGrace = 5 * time.Second

// shutdownTimeout reads SHUTDOWN_TIMEOUT (time.ParseDuration format), falling
// back to defaultShutdownTimeout when unset, malformed, or not strictly
// positive. Delegates to server.EnvDuration — the internal/server package
// already needs this exact contract for its own ANSWER_*_TIMEOUT env vars —
// so the positive-duration validation lives in exactly one place rather than
// being duplicated here.
func shutdownTimeout() time.Duration {
	return server.EnvDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout)
}

func runServe(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	dbURL := mustEnv("DATABASE_URL")
	addr, _ := cmd.Flags().GetString("addr")

	st, err := store.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer st.Close()

	handler, err := server.New(st, embedder.New(), llm.New())
	if err != nil {
		return fmt.Errorf("configure server: %w", err)
	}

	// baseCtx is the ultimate parent of every request's r.Context() (see
	// BaseContext below). It stays live through Shutdown's whole drain
	// window, so /api/ask and MCP tool calls keep running normally — bounded
	// by their own ANSWER_TOTAL_BUDGET, not cut off early. It is cancelled
	// only as a last resort, once Shutdown has already given in-flight
	// requests their full SHUTDOWN_TIMEOUT and failed (see the
	// DeadlineExceeded branch below) — that is what actually stops a
	// handler that ignores Shutdown's own signal from running forever after
	// the listener is gone.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	// MCP tool calls do not descend from baseCtx (the go-sdk strips
	// cancellation from their context), so every path that cancels baseCtx
	// must call handler.ForceCancel too — including this deferred one, so no
	// return from runServe leaves a tool call running against a closed store.
	defer handler.ForceCancel()

	httpSrv := &http.Server{
		Addr:    addr,
		Handler: handler,
		BaseContext: func(net.Listener) context.Context {
			return baseCtx
		},
		// No WriteTimeout: it would apply to the whole response including
		// the body, and SSE responses (/api/ask, /api/mcp) legitimately
		// stream for 60s+. ReadHeaderTimeout and IdleTimeout already bound a
		// slow or idle client without touching an active response.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// The MCP GET stream (internal/server's mcpHandler) blocks on its
	// request context with no timeout of its own — that is what a
	// server-to-client stream is. Left alone it would hold up Shutdown's
	// drain for the full SHUTDOWN_TIMEOUT even though it carries no
	// in-progress answer to finish, unlike /api/ask or an MCP tool call.
	// RegisterOnShutdown fires the instant Shutdown is called, so that
	// stream closes immediately while everything else keeps draining.
	httpSrv.RegisterOnShutdown(handler.CancelStreams)

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("Formance Docs RAG — http://localhost%s\n", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Printf("shutting down: %v", ctx.Err())
	}

	// signal.NotifyContext (cmd.Execute) keeps intercepting SIGINT/SIGTERM
	// until its stop func runs, which only happens once runServe returns —
	// so without this a second Ctrl-C during the drain below would be
	// swallowed. Resetting to the default disposition makes a second signal
	// kill the process immediately, the usual "press Ctrl-C again to force
	// quit" behaviour. Reset rather than plumbing Execute's stop func down:
	// same effect, no API change to cmd.Execute. The first signal has
	// already done its job (ctx is cancelled), so nothing is lost.
	signal.Reset(os.Interrupt, syscall.SIGTERM)

	// ctx is already cancelled (that's why we're here), so the shutdown
	// deadline is derived from a fresh, uncancelled context instead —
	// otherwise Shutdown would return immediately rather than draining
	// in-flight requests for up to shutdownTimeout().
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout())
	defer cancel()
	shutdownErr := httpSrv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		log.Printf("http shutdown: %v", shutdownErr)
		// Usually DeadlineExceeded: every in-flight request already got its
		// full SHUTDOWN_TIMEOUT to finish gracefully and at least one
		// didn't. (Any other Shutdown error is treated the same way: the
		// graceful drain did not complete.) Cancelling baseCtx reaches
		// every HTTP request's context — it's the root every one derives
		// from, see BaseContext above — and ForceCancel reaches MCP tool
		// calls, whose go-sdk context does not descend from it. Every
		// pipeline stage (LLM/embedder calls, DB queries) respects ctx, so
		// what is still running unblocks promptly instead of continuing
		// unbounded after the listener is gone. Close then drops the
		// underlying connections outright: the graceful path already had
		// its chance.
		cancelBase()
		handler.ForceCancel()
		if err := httpSrv.Close(); err != nil {
			log.Printf("http close: %v", err)
		}
	}

	// Give any handler or MCP tool call still unwinding after the
	// cancellation above a short, bounded grace period to actually return —
	// it still needs to make its logRequest call — before draining
	// request-log writes and closing the DB below. WaitHandlers also stops
	// admitting new work first: a request that still slips in gets 503, a
	// tool call a "server shutting down" tool error.
	if !handler.WaitHandlers(handlerGrace) {
		log.Printf("http shutdown: handler(s) still running after %s grace period, proceeding anyway", handlerGrace)
	}

	// Drain in-flight request-log writes before the deferred st.Close()
	// above runs — each write already bounds itself (see logWriteTimeout in
	// internal/server), so this returns promptly. Wait also marks the
	// server as closing before it starts, so no write that starts after
	// this point can race with it.
	handler.Wait()

	return <-serveErr
}
