// Package health answers whether this process is fit to serve.
package health

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Handler reports readiness by checking the schema the running binary expects,
// not by pinging the database.
//
// The difference is the whole point, and it is inherited from a silent outage
// in the sibling project. A rollback restores the binary and not the database.
// The old binary's CREATE TABLE IF NOT EXISTS is a no-op, so it starts
// cleanly; its health check was a ping, so the probe answered 200; and every
// real request then failed on a column that no longer existed. The deploy saw
// a healthy service and kept it.
//
// So a deploy that rolls the binary back onto a newer schema must fail *here*,
// loudly, while the previous binary is still one rename away.
//
// The response body is deliberately incurious. /healthz is reachable by
// anybody, so the detail -- which table, which column -- goes to the log,
// where the person debugging it can see it and a stranger cannot.
func Handler(log *slog.Logger, db *sql.DB, want sqldb.Expected) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := sqldb.CheckSchema(r.Context(), db, want); err != nil {
			log.ErrorContext(r.Context(), "health check failed",
				"err", err,
				"hint", "the database does not match this binary; if a deploy just rolled back, the schema is newer than the code")

			http.Error(w, "not ready", http.StatusServiceUnavailable)

			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	}
}
