package session

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/routing"
)

// Allocation contains the concrete session keys selected for a routed turn.
// The current implementation intentionally preserves the legacy session-key
// layout while moving key construction out of the router.
type Allocation struct {
	SessionKey     string
	MainSessionKey string
}

// AllocationInput contains the routing result and peer context needed to
// derive the session keys for a turn.
type AllocationInput struct {
	AgentID       string
	Channel       string
	AccountID     string
	Peer          *routing.RoutePeer
	SessionPolicy routing.SessionPolicy
}

// AllocateRouteSession maps a route decision onto the current legacy
// agent-scoped session-key format.
func AllocateRouteSession(input AllocationInput) Allocation {
	sessionKey := strings.ToLower(routing.BuildAgentPeerSessionKey(routing.SessionKeyParams{
		AgentID:       input.AgentID,
		Channel:       input.Channel,
		AccountID:     input.AccountID,
		Peer:          input.Peer,
		DMScope:       input.SessionPolicy.DMScope,
		IdentityLinks: input.SessionPolicy.IdentityLinks,
	}))
	mainSessionKey := strings.ToLower(routing.BuildAgentMainSessionKey(input.AgentID))
	return Allocation{
		SessionKey:     sessionKey,
		MainSessionKey: mainSessionKey,
	}
}
