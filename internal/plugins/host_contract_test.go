package plugins

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-bot/internal/credentials"
)

func TestHostBridgeCredentialNotificationUsesPublicMetadata(t *testing.T) {
	r := bridgeFixture(t, `export const inject=['host'];export async function apply(ctx){const changes=[];ctx.on('host/credential-changed',event=>changes.push(event));await ctx.host.tools.register({name:'changes',description:'Read received changes',inputSchema:{type:'object'},async execute(){return{content:[{type:'text',text:JSON.stringify(changes)}]}}})}`)
	if _, err := r.Apply(t.Context(), []hostPluginInstance{{ID: "mail", Package: "fixture", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	secret := "private-notification-token"
	if _, err := r.config.Credentials.ModifyRecord(t.Context(), "mail/mailbox", func(*credentials.Record) (*credentials.Record, error) {
		return &credentials.Record{Kind: "api-key", Key: &secret}, nil
	}); err != nil {
		t.Fatal(err)
	}
	tools, err := r.Tools(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("credential notification did not reach the Node listener")
		case <-tick.C:
			text, err := tools[0].Execute(t.Context(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(text, secret) {
				t.Fatal("notification leaked the credential")
			}
			if text == "[]" {
				continue
			}
			var received []struct {
				InstanceID string `json:"instanceId"`
				Key        string `json:"key"`
				Configured bool   `json:"configured"`
			}
			if json.Unmarshal([]byte(text), &received) != nil || len(received) != 1 || received[0].InstanceID != "mail" || received[0].Key != "mailbox" || !received[0].Configured {
				t.Fatal(text)
			}
			return
		}
	}
}

func TestHostBridgeToolResultRejectsUnpublishedAssetAndFactFields(t *testing.T) {
	for _, body := range []string{
		`{content:[{type:'image',image:{id:'forged'}}]}`,
		`{content:[{type:'file',file:{path:'/root/harness/report.txt'}}]}`,
		`{content:[{type:'text',text:'ok'}],meta:{presentedFiles:['/root/harness/report.txt']}}`,
	} {
		t.Run(body, func(t *testing.T) {
			r := bridgeFixture(t, `export const inject=['host'];export async function apply(ctx){if(ctx.host.assets!==undefined||ctx.host.tools.registerGuard!==undefined)throw Error('removed API is present');await ctx.host.tools.register({name:'result',description:'Result contract',inputSchema:{type:'object'},async execute(){return `+body+`}})}`)
			status, err := r.Apply(t.Context(), []hostPluginInstance{{ID: "mail", Package: "fixture", Enabled: true}})
			if err != nil || status[0]["state"] != "ready" {
				t.Fatalf("load=%v err=%v", status, err)
			}
			tools, err := r.Tools(t.Context(), "session")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tools[0].Execute(t.Context(), json.RawMessage(`{}`)); err == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}
}

func TestHostStateNotificationsAreBoundedAndCoalesced(t *testing.T) {
	r := &hostBridge{closed: make(chan struct{}), notifications: hostNotifications{pending: map[string]hostNotification{}, wake: make(chan struct{}, 1)}}
	for index := 0; index < 70; index++ {
		key := fmt.Sprint(index)
		r.notify("host/credential-changed", key, map[string]int{"revision": index})
	}
	if len(r.notifications.pending) != 64 || len(r.notifications.order) != 64 || r.notifications.dropped != 6 {
		t.Fatal("notification producer was not bounded")
	}
	r.notify("host/credential-changed", "69", map[string]int{"revision": 999})
	if len(r.notifications.order) != 64 || r.notifications.pending["69"].data.(map[string]int)["revision"] != 999 {
		t.Fatal("notification was not coalesced")
	}
	close(r.closed)
	r.notify("host/skills-changed", "after-close", nil)
	if len(r.notifications.order) != 64 {
		t.Fatal("closed runtime accepted a notification")
	}
}

func TestHostBridgeHandshakeConfirmsVersionLimitsAndServices(t *testing.T) {
	r := &hostBridge{}
	for _, raw := range []string{`{"protocol":2,"api":"1"}`, `{"protocol":1,"api":"2"}`} {
		if _, err := r.handle(t.Context(), "bridge.initialize", json.RawMessage(raw)); err == nil {
			t.Fatal("accepted incompatible peer", raw)
		}
	}
	result, err := r.handle(t.Context(), "bridge.initialize", json.RawMessage(`{"protocol":1,"api":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	view := result.(map[string]any)
	if view["maxFrameBytes"] != maxFrame {
		t.Fatal(view)
	}
	services := view["capabilities"].([]string)
	if strings.Join(services, ",") != "info,tools,skills,prompts,credentials,authorization,mcp,approvals,questions" {
		t.Fatal(services)
	}
}
