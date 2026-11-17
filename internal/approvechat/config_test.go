package approvechat_test

import (
	"strings"
	"testing"

	"github.com/spf13/viper"

	"hop.top/heo/internal/approvechat"
)

func TestLoad(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	err := v.ReadConfig(strings.NewReader(`
held_operations:
  chat:
    enabled: true
    conversations:
      - inbox: in1
        conversation: cv1
        slack_thread_replies: true
        members:
          - {identity: idn-alice, actor: "uid:1001"}
`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := approvechat.Load(v)
	if err != nil || !c.Enabled || len(c.Conversations) != 1 || c.Conversations[0].Members[0].Actor != "uid:1001" {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	if !c.Conversations[0].SlackThreadReplies {
		t.Fatal("slack_thread_replies not read")
	}
	if c, err := approvechat.Load(viper.New()); err != nil || c.Enabled {
		t.Fatalf("absent = %+v, %v", c, err)
	}
}

func TestValidate(t *testing.T) {
	mk := func(m ...approvechat.Member) approvechat.Config { return cfg(m...) }
	cases := map[string]struct {
		c    approvechat.Config
		want string
	}{
		"plugin actor": {mk(approvechat.Member{Identity: "i", Actor: "plugin:ops"}), "plugin actor"},
		"no identity":  {mk(approvechat.Member{Actor: "uid:1"}), "identity: required"},
		"twice mapped": {mk(alice, alice), "mapped twice"},
		"no members":   {mk(), "no members"},
		"no chats":     {approvechat.Config{Enabled: true}, "no conversations"},
		"bad actor":    {mk(approvechat.Member{Identity: "i", Actor: ""}), "actor"},
	}
	for name, tc := range cases {
		err := tc.c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if err := cfg(alice, bob).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGrammar(t *testing.T) {
	cases := []struct {
		text    string
		inReply bool
		want    approvechat.Command
		ok      bool
	}{
		{"approve", true, approvechat.Command{Decision: "approve"}, true},
		{"Reject: too risky", true, approvechat.Command{Decision: "reject", Note: "too risky"}, true},
		{"reject #op9 no", false, approvechat.Command{Decision: "reject", Operation: "op9", Note: "no"}, true},
		{"approve", false, approvechat.Command{}, false},
		{"approved already", true, approvechat.Command{}, false},
		{"", true, approvechat.Command{}, false},
	}
	for _, tc := range cases {
		got, ok := approvechat.ParseReply(tc.text, tc.inReply)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseReply(%q,%v) = %+v,%v", tc.text, tc.inReply, got, ok)
		}
	}
	for e, want := range map[string]string{"👍": "approve", "✅": "approve", "👎": "reject", "❌": "reject", "👍️": "approve", "👍🏽": ""} {
		got, ok := approvechat.ParseReaction(e)
		if got.Decision != want || ok != (want != "") {
			t.Errorf("ParseReaction(%q) = %+v,%v", e, got, ok)
		}
	}
}
