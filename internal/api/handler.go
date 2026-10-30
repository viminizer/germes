package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
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
	mux.HandleFunc("GET /sessions/{id}/checkpoints", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) { return svc.ListCheckpoints(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("POST /sessions/{id}/checkpoints/{cp}/restore", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) {
			return nil, svc.RestoreCheckpoint(r.Context(), r.PathValue("id"), r.PathValue("cp"))
		})
	})
	mux.HandleFunc("GET /insights", func(w http.ResponseWriter, r *http.Request) {
		since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		if err != nil {
			writeError(w, NewError(CodeInvalid, "since: want an RFC 3339 time"))
			return
		}
		reply(w, r, func() (any, error) { return svc.GetInsights(r.Context(), since) })
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
	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		var j JobRequest
		if !decode(w, r, &j) {
			return
		}
		reply(w, r, func() (any, error) { return svc.CreateJob(r.Context(), j) })
	})
	mux.HandleFunc("GET /jobs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := JobFilter{Status: JobStatus(q.Get("status")), Limit: atoi(q.Get("limit")), Cursor: q.Get("cursor")}
		reply(w, r, func() (any, error) { return svc.ListJobs(r.Context(), f) })
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) { return svc.GetJob(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("POST /jobs/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) { return nil, svc.StartJob(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("POST /jobs/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) { return nil, svc.StopJob(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("DELETE /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) { return nil, svc.DeleteJob(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, func() (any, error) { return svc.ListTools(r.Context(), r.URL.Query().Get("session")) })
	})
	mux.HandleFunc("POST /tools/run", func(w http.ResponseWriter, r *http.Request) {
		var t ToolRequest
		if !decode(w, r, &t) {
			return
		}
		reply(w, r, func() (any, error) { return svc.RunTool(r.Context(), t) })
	})
	mux.HandleFunc("POST /tools/check", func(w http.ResponseWriter, r *http.Request) {
		var t ToolRequest
		if !decode(w, r, &t) {
			return
		}
		reply(w, r, func() (any, error) { return svc.CheckTool(r.Context(), t) })
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
