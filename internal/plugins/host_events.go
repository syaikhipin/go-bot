package plugins

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"go-bot/internal/credentials"
)

// State notifications are bounded, coalesced observations. They never hold up
// a credential write, callback, tool result or Session commit.
type hostNotifications struct {
	mu      sync.Mutex
	pending map[string]hostNotification
	order   []string
	wake    chan struct{}
	dropped uint64
}
type hostNotification struct {
	name string
	data any
}

func (r *hostBridge) notify(name, key string, data any) {
	n := &r.notifications
	n.mu.Lock()
	select {
	case <-r.closed:
		n.mu.Unlock()
		return
	default:
	}
	if _, exists := n.pending[key]; !exists {
		if len(n.order) == 64 {
			delete(n.pending, n.order[0])
			n.order = n.order[1:]
			n.dropped++
		}
		n.order = append(n.order, key)
	}
	n.pending[key] = hostNotification{name, data}
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (r *hostBridge) startNotifications() {
	n := &r.notifications
	n.pending, n.wake = make(map[string]hostNotification), make(chan struct{}, 1)
	if r.config.Credentials != nil {
		r.unsubscribers = append(r.unsubscribers, r.config.Credentials.Subscribe(func(update credentials.Update) {
			for _, address := range update.Records {
				owner, key, valid := strings.Cut(address, "/")
				if !valid || !bridgeInstanceName.MatchString(owner) {
					continue
				}
				info, err := r.config.Credentials.DescribeRecord(address)
				if err == nil {
					r.notify("host/credential-changed", "credential:"+address, map[string]any{"instanceId": owner, "key": key, "configured": info.Configured})
				}
			}
		}))
	}
	if r.config.Authorization != nil {
		r.unsubscribers = append(r.unsubscribers, r.config.Authorization.Subscribe(func(address, phase string) {
			owner, key, valid := strings.Cut(address, "/")
			if valid && bridgeInstanceName.MatchString(owner) {
				r.notify("host/authorization-changed", "authorization:"+address, map[string]string{"instanceId": owner, "key": key, "phase": phase})
			}
		}))
	}
	go func() {
		for {
			select {
			case <-r.closed:
				return
			case <-n.wake:
			}
			for {
				n.mu.Lock()
				if len(n.order) == 0 {
					n.mu.Unlock()
					break
				}
				key := n.order[0]
				n.order = n.order[1:]
				event := n.pending[key]
				delete(n.pending, key)
				dropped := n.dropped
				n.dropped = 0
				n.mu.Unlock()
				if dropped > 0 && r.config.Log != nil {
					r.config.Log("warn", fmt.Sprintf("Host state notification queue dropped %d older entries; query current state to refresh", dropped))
				}
				data, err := json.Marshal(map[string]any{"name": event.name, "data": event.data})
				if err == nil {
					err = r.peer.send(frame{JSONRPC: "2.0", Method: "host.event", Params: data})
				}
				if err != nil {
					return
				}
			}
		}
	}()
	if r.config.MCP != nil {
		go func() {
			for {
				changed, done := r.config.MCP.Changes()
				select {
				case <-r.closed:
					return
				case <-done:
					return
				case <-changed:
				}
				for _, row := range r.rows("mcp") {
					var definition hostMCPDefinition
					_ = json.Unmarshal(row.Descriptor, &definition)
					for _, view := range r.mcpViews(row.Owner) {
						if view["name"] == definition.Name {
							payload := map[string]any{"instanceId": row.Owner, "name": definition.Name, "state": view["state"], "toolCount": view["toolCount"]}
							if message, ok := view["error"]; ok {
								payload["error"] = message
							}
							r.notify("host/mcp-changed", "mcp:"+row.ID, payload)
						}
					}
				}
			}
		}()
	}
}
func (r *hostBridge) contributionChanged(row *hostContribution) {
	if strings.HasPrefix(row.Kind, "skill") {
		r.notify("host/skills-changed", "skill:"+row.Owner, map[string]string{"instanceId": row.Owner, "source": row.Kind})
	}
	if row.Kind == "mcp" {
		var definition hostMCPDefinition
		_ = json.Unmarshal(row.Descriptor, &definition)
		payload := map[string]any{"instanceId": row.Owner, "name": definition.Name, "state": "removed", "toolCount": 0}
		if row.ctx.Err() == nil {
			for _, view := range r.mcpViews(row.Owner) {
				if view["name"] == definition.Name {
					payload["state"], payload["toolCount"] = view["state"], view["toolCount"]
				}
			}
		}
		r.notify("host/mcp-changed", "mcp:"+row.ID, payload)
	}
	if r.config.Changed != nil {
		r.config.Changed()
	}
}
