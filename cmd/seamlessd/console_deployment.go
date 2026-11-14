package main

import (
	"maps"
	"os/user"
	"slices"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
)

// consoleDeployment describes how this daemon runs for the console's System
// and Privacy screens: facts the console cannot read from a request. bind is
// the address the listener took, tools the MCP tools the owner is offered,
// and chat whether a chat model is configured.
func consoleDeployment(cfg config.Config, bind string, tools int, chat bool) console.Deployment {
	d := console.Deployment{
		Addr:          bind,
		PeerAuth:      cfg.PeerAuth.Enabled,
		MCPTools:      tools,
		CommitsRecord: cfg.Commits.Record,
		FeedSocket:    cfg.Screening.FeedSocket != "",
	}
	if u, err := user.Current(); err == nil {
		d.DaemonUser = u.Username
	}
	d.Owner = d.DaemonUser
	if cfg.PeerAuth.Enabled {
		d.SocketPath = cfg.PeerAuth.Socket.Path
		if cfg.PeerAuth.Owner != "" {
			d.Owner = cfg.PeerAuth.Owner
		}
		d.Restricted = slices.Clone(cfg.PeerAuth.Restricted)
		for _, relay := range slices.Sorted(maps.Keys(cfg.PeerAuth.Relays)) {
			for _, name := range slices.Sorted(maps.Keys(cfg.PeerAuth.Relays[relay].Restricted)) {
				d.Restricted = append(d.Restricted, access.RelayAccountLabel(name, relay))
			}
		}
	}
	if chat {
		d.ChatModel = chatModel(cfg.LLM)
	}
	return d
}

// chatModel is the chat model the configured provider uses, or "" for an
// unknown provider.
func chatModel(l config.LLM) string {
	switch l.Provider {
	case "openai":
		return l.OpenAI.ChatModel
	case "ollama":
		return l.Ollama.ChatModel
	case "anthropic":
		return l.Anthropic.ChatModel
	}
	return ""
}
