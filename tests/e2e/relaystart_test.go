package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

// TestRelayStartPreviewMatchesSend: on the release build's relay.messages
// channel, `message send --dry-run` names the recipient the real send
// uses. An Apple ID's email address in any letter case is previewed
// lower-cased (the spelling the gateway is sent, and the inbound
// reports), the typed spelling beside it as "as typed", in text and JSON
// and with the flag in either position; a lower-case address, a number,
// and every address in the SMS and WhatsApp namespaces are previewed as
// typed, with no "as typed". The real send's destination is the previewed
// one, and the gateway is sent that address.
func TestRelayStartPreviewMatchesSend(t *testing.T) {
	withE2EBinary(t, func() {
		gw := newRelayGatewayServer(t)
		c := relayCLISite(t, gw.host(), true)
		d := c.start()
		var ch channelOutJSON
		c.json(&ch, "channel", "create", "--type", string(model.TypeRelayMessages), "--key", "imessage", "--credential-ref", "gw",
			"--configuration", relayChannelJSON("imessage", gw.srv.URL+"/uma/reply", gw.srv.URL+"/uma/lookup"))
		c.heo(0, "inbox", "create", "--channel", "imessage", "--key", "imessages", "--destination", relayLine,
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		secret := func(v string) string {
			b, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": v})
			return string(b)
		}
		c.s.write("secrets/uma/default/gw@relay.webhook", secret(relayWebhookSecret))
		c.s.write("secrets/uma/default/gw@relay.callback", secret(relayCallbackSecret))
		send := func(to string) []string {
			return []string{"message", "send", "--inbox", "imessages", "--text", "welcome", "--to", to}
		}

		for _, tc := range []struct {
			name string
			to   string
			// want is the destination the preview names; differs is
			// whether it says the typed spelling too.
			want    model.ExternalRef
			differs bool
		}{
			{"mixed-case Apple ID", "relay.imessage/phone:Bob@Example.com",
				model.ExternalRef{Channel: ch.Channel.Ref, Namespace: relay.NamespaceIMessage, Kind: "phone", ID: "bob@example.com"}, true},
			{"lower-case Apple ID", "relay.imessage/phone:carol@example.com",
				model.ExternalRef{Channel: ch.Channel.Ref, Namespace: relay.NamespaceIMessage, Kind: "phone", ID: "carol@example.com"}, false},
			{"phone number", "relay.imessage/phone:+15550100003",
				model.ExternalRef{Channel: ch.Channel.Ref, Namespace: relay.NamespaceIMessage, Kind: "phone", ID: "+15550100003"}, false},
			{"mixed-case address in the SMS namespace", "relay.sms/phone:Dan@Example.com",
				model.ExternalRef{Channel: ch.Channel.Ref, Namespace: relay.NamespaceSMS, Kind: "phone", ID: "Dan@Example.com"}, false},
			{"mixed-case address in the WhatsApp namespace", "relay.whatsapp/phone:Eve@Example.com",
				model.ExternalRef{Channel: ch.Channel.Ref, Namespace: relay.NamespaceWhatsApp, Kind: "phone", ID: "Eve@Example.com"}, false},
		} {
			typedID := tc.to[strings.LastIndex(tc.to, ":")+1:]

			// The preview, text, with the flag in either position.
			for _, r := range c.sendPreviews(0, send(tc.to)...) {
				if !strings.Contains(r.stdout, "destination   phone "+tc.want.ID) {
					t.Errorf("%s: text preview lacks destination %q:\n%s", tc.name, tc.want.ID, r.stdout)
				}
				m := asTypedRow.FindStringSubmatch(r.stdout)
				switch {
				case tc.differs && (m == nil || m[1] != "phone" || m[2] != typedID):
					t.Errorf("%s: text preview lacks the typed spelling %q as a row:\n%s", tc.name, typedID, r.stdout)
				case !tc.differs && strings.Contains(r.stdout, "as typed"):
					t.Errorf("%s: text preview shows a typed spelling that does not differ:\n%s", tc.name, r.stdout)
				}
				if tc.differs && strings.Count(r.stdout, typedID) != 1 {
					t.Errorf("%s: the typed spelling shows more than once in the text preview:\n%s", tc.name, r.stdout)
				}
			}

			// The preview, JSON, with the flag in either position.
			var plan planJSON
			for _, args := range [][]string{append(send(tc.to), "--dry-run"), append([]string{"--dry-run"}, send(tc.to)...)} {
				c.json(&plan, args...)
				var dest model.ExternalRef
				if err := json.Unmarshal(plan.Args["destination"], &dest); err != nil || dest != tc.want {
					t.Errorf("%s: JSON preview %v destination = %s, want %+v", tc.name, args, plan.Args["destination"], tc.want)
				}
				asTyped, has := routeID(t, plan, "as_typed")
				if has != tc.differs || (tc.differs && asTyped != typedID) {
					t.Errorf("%s: JSON preview %v as_typed = %q (%v), want %q (%v)", tc.name, args, asTyped, has, typedID, tc.differs)
				}
			}

			// The real send is routed where the preview said.
			var sent sendJSON
			c.json(&sent, send(tc.to)...)
			if sent.Destination != tc.want {
				t.Errorf("%s: send destination = %+v, want the previewed %+v", tc.name, sent.Destination, tc.want)
			}
			got := c.waitDelivery(sent.Operation.ID, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected)
			if tc.want.Namespace == relay.NamespaceIMessage {
				if got.State != model.ProviderAccepted && got.State != model.Delivered {
					t.Fatalf("%s: send = %s %+v", tc.name, got.State, got.Failure)
				}
				cbs := gw.callbacks()
				if last := cbs[len(cbs)-1]; last["to"] != tc.want.ID {
					t.Errorf("%s: the gateway was sent %v, want %q", tc.name, last["to"], tc.want.ID)
				}
			}
		}
		// The channel is iMessage: the sends in the other namespaces never
		// reached the gateway.
		if n := len(gw.callbacks()); n != 3 {
			t.Errorf("the gateway took %d callbacks, want the three iMessage sends", n)
		}
		c.stop(d)
	})
}
