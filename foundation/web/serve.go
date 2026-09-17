package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Surface is one listener: a name for the logs, an address, and what answers.
//
// This binary runs two of them. That is not load balancing -- it is how the
// embedded form and the management app are told apart. Apache's
// ProxyPreserveHost cannot be set in .htaccess, which is all konsoleH offers,
// so a proxied request arrives with Host: 127.0.0.1:<port> whatever the visitor
// typed. Deciding the surface by the socket that accepted the connection is
// therefore not merely convenient: it is the only option that is not forgeable,
// and it means the management mux is not reachable from the embed vhost at all.
// See docs/design/drop-in-forms.md section 3.1.
type Surface struct {
	Name    string
	Addr    string
	Handler http.Handler
}

// Timeouts a server is built with. Every one of them is set, because a server
// with no timeouts is a server one slow client can hold open.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 90 * time.Second
)

// Serve runs every surface until the context is cancelled or a listener fails,
// then shuts them all down within grace.
//
// Every socket is bound before any of them is served, and that ordering is the
// point of the function. ListenAndServe binds and serves in one call from
// inside a goroutine, so a port already in use is discovered *after* the log
// line claiming to be listening -- which is exactly how a process that is
// about to die reports success. Binding first turns "the admin port was taken"
// into a startup error, on stderr, with a non-zero exit, while the supervisor
// still has the previous binary one rename away.
//
// A listener that cannot bind takes the whole process with it. Carrying on
// with whichever surface came up is the genuinely dangerous outcome: if the
// embed socket binds and the management one does not, the service looks healthy
// while half of it is missing, and the other way round leaves the management
// app answering on the port the embed vhost proxies to.
func Serve(ctx context.Context, log *slog.Logger, grace time.Duration, surfaces ...Surface) error {
	if len(surfaces) == 0 {
		return errors.New("no listeners were configured; check the [server] section of the config file")
	}

	srvs := make([]*http.Server, len(surfaces))
	lns := make([]net.Listener, len(surfaces))

	for i, s := range surfaces {
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			// Undo the binds that did succeed, so a failed start leaves no
			// socket held by a process that is exiting.
			for _, open := range lns[:i] {
				open.Close()
			}

			return fmt.Errorf("the %s listener could not take %s: %w", s.Name, s.Addr, err)
		}

		lns[i] = ln
		srvs[i] = &http.Server{
			Handler:           s.Handler,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,

			// net/http's own complaints go through the same logger as
			// everything else, rather than to a bare stderr that nothing is
			// tailing.
			ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
	}

	// Buffered by the number of listeners, so a second failure during shutdown
	// cannot block the goroutine reporting it.
	fatal := make(chan error, len(surfaces))

	var wg sync.WaitGroup
	for i, s := range surfaces {
		srv, ln, name := srvs[i], lns[i], s.Name

		log.Info("listening", "surface", name, "addr", ln.Addr().String())

		wg.Go(func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fatal <- fmt.Errorf("%s listener on %s: %w", name, ln.Addr(), err)
			}
		})
	}

	var serveErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down", "reason", "signal")
	case serveErr = <-fatal:
		log.Error("shutting down", "reason", "a listener failed", "err", serveErr)
	}

	// WithoutCancel, because by here the parent context is usually the one
	// that was just cancelled -- and a shutdown deadline derived from an
	// already-cancelled context gives in-flight requests no grace at all.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	for i, s := range surfaces {
		if err := srvs[i].Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown did not finish in time", "surface", s.Name, "err", err)
			srvs[i].Close()
		}
	}

	wg.Wait()

	return serveErr
}
