package session

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/routing"
)

func TestAllocateRouteSession_PerPeerDM(t *testing.T) {
	allocation := AllocateRouteSession(AllocationInput{
		AgentID:   "main",
		Channel:   "telegram",
		AccountID: "default",
		Peer: &routing.RoutePeer{
			Kind: "direct",
			ID:   "User123",
		},
		SessionPolicy: routing.SessionPolicy{
			DMScope: routing.DMScopePerPeer,
		},
	})

	if allocation.SessionKey != "agent:main:direct:user123" {
		t.Fatalf("SessionKey = %q, want %q", allocation.SessionKey, "agent:main:direct:user123")
	}
	if allocation.MainSessionKey != "agent:main:main" {
		t.Fatalf("MainSessionKey = %q, want %q", allocation.MainSessionKey, "agent:main:main")
	}
}

func TestAllocateRouteSession_GroupPeer(t *testing.T) {
	allocation := AllocateRouteSession(AllocationInput{
		AgentID:   "main",
		Channel:   "slack",
		AccountID: "workspace-a",
		Peer: &routing.RoutePeer{
			Kind: "channel",
			ID:   "C001",
		},
		SessionPolicy: routing.SessionPolicy{
			DMScope: routing.DMScopePerAccountChannelPeer,
		},
	})

	if allocation.SessionKey != "agent:main:slack:channel:c001" {
		t.Fatalf("SessionKey = %q, want %q", allocation.SessionKey, "agent:main:slack:channel:c001")
	}
	if allocation.MainSessionKey != "agent:main:main" {
		t.Fatalf("MainSessionKey = %q, want %q", allocation.MainSessionKey, "agent:main:main")
	}
}
