package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
)

// NewHandler serves svc over HTTP. Watch streams newline-delimited JSON.
func NewHandler(svc Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /daemon", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) { return svc.GetDaemon(r.Context()) })
	})
	mux.HandleFunc("POST /daemon/stop", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) { return nil, svc.StopDaemon(r.Context()) })
	})
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := SessionFilter{Project: q.Get("project"), Status: Status(q.Get("status")), Limit: atoi(q.Get("limit")), Cursor: q.Get("cursor")}
		reply(w, r, func() (any, error) { return svc.ListSessions(r.Context(), f) })
	})
	mux.HandleFunc("GET /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) { return svc.GetSession(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("PATCH /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var u SessionUpdate
		if !decode(w, r, &u) {
			return
		}
		u.ID = r.PathValue("id")
		reply(w, r, func() (any, error) { return nil, svc.UpdateSession(r.Context(), u) })
	})
	mux.HandleFunc("DELETE /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) { return nil, svc.DeleteSession(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("POST /sessions/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		var s StopRequest
		if !decode(w, r, &s) {
			return
		}
		s.ID = r.PathValue("id")
		reply(w, r, func() (any, error) { return nil, svc.StopSession(r.Context(), s) })
	})
	mux.HandleFunc("POST /messages", func(w http.ResponseWriter, r *http.Request) {
		var m MessageRequest
		if !decode(w, r, &m) {
			return
		}
		reply(w, r, func() (any, error) { return svc.SendMessage(r.Context(), m) })
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := EventFilter{SessionID: q.Get("session"), AfterSeq: atoi64(q.Get("after")), Limit: atoi(q.Get("limit"))}
		reply(w, r, func() (any, error) { return svc.ListEvents(r.Context(), f) })
	})
	mux.HandleFunc("GET /watch", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ch, err := svc.Watch(r.Context(), WatchFilter{SessionID: q.Get("session"), AfterSeq: atoi64(q.Get("after"))})
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		enc := json.NewEncoder(w)
		for ev := range ch {
			if err := enc.Encode(ev); err != nil {
				return // client went away
			}
			// Flush every event, or text arrives in chunks.
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	return mux
}

func reply(w http.ResponseWriter, r *http.Request, call func() (any, error)) {
	v, err := call()
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if v == nil {
		v = struct{}{}
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write reply", "path", r.URL.Path, "err", err)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, NewError(CodeInvalid, "bad request body: %v", err))
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Code: CodeInternal, Message: err.Error()}
	}
	status := map[Code]int{
		CodeNotFound: http.StatusNotFound, CodeInvalid: http.StatusBadRequest,
		CodeUnauthorized: http.StatusUnauthorized, CodeVersionMismatch: http.StatusConflict,
		CodeUnavailable: http.StatusServiceUnavailable,
	}[e.Code]
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(e)
}

func atoi(s string) int     { n, _ := strconv.Atoi(s); return n }
func atoi64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
