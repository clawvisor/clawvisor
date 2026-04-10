package auth

// TicketStorer is the interface for SSE ticket stores. The in-memory
// TicketStore and RedisTicketStore both implement it.
type TicketStorer interface {
	Generate(userID string) (string, error)
	Validate(ticket string) (string, error)
	Cleanup()
}
