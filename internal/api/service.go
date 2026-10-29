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

// EventLister lists stored events (the Service, or the store).
type EventLister interface {
	ListEvents(ctx context.Context, f EventFilter) (Page[Event], error)
}

// ListAllEvents reads every stored event of f.SessionID after f.AfterSeq,
// page by page.
func ListAllEvents(ctx context.Context, l EventLister, f EventFilter) ([]Event, error) {
	var events []Event
	after := f.AfterSeq
	for {
		page, err := l.ListEvents(ctx, EventFilter{SessionID: f.SessionID, AfterSeq: after, Limit: 1000})
		if err != nil {
			return nil, err
		}
		events = append(events, page.Items...)
		if page.Next == "" || len(page.Items) == 0 {
			return events, nil
		}
		after = page.Items[len(page.Items)-1].Seq
	}
}
