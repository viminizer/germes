package api

import "context"

// Service is everything a client can ask Germes to do. The daemon implements
// it; Client calls it over the socket.
type Service interface {
	GetDaemon(ctx context.Context) (Daemon, error)
	StopDaemon(ctx context.Context) error

	ListSessions(ctx context.Context, f SessionFilter) (Page[Session], error)
	GetSession(ctx context.Context, id string) (Session, error)
	UpdateSession(ctx context.Context, u SessionUpdate) error
	DeleteSession(ctx context.Context, id string) error
	SendMessage(ctx context.Context, r MessageRequest) (MessageResult, error)
	StopSession(ctx context.Context, r StopRequest) error

	ListEvents(ctx context.Context, f EventFilter) (Page[Event], error)
	Watch(ctx context.Context, f WatchFilter) (<-chan Event, error)
}
