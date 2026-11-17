package approvechat_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"hop.top/heo/internal/approvechat"
)

func TestFileStoreRoundTrips(t *testing.T) {
	dir := t.TempDir()
	st := approvechat.NewFileStore(dir)
	if got, err := st.Load(); err != nil || !reflect.DeepEqual(got, approvechat.State{}) {
		t.Fatalf("empty Load = %+v, %v", got, err)
	}
	want := approvechat.State{
		Announcements: []approvechat.Announced{{Conversation: "in1/cv1", Message: "m1", Operation: "op1"}},
		Handled:       []string{"e1"},
	}
	if err := st.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Load = %+v, %v; want %+v", got, err, want)
	}
	fi, err := os.Stat(filepath.Join(dir, "approval-chat.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file = %v, %v; want owner-only", fi, err)
	}
}

// TestUnreadableStateStartsEmpty: a corrupt file is logged, not fatal.
func TestUnreadableStateStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "approval-chat.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRig(cfg(alice))
	r.h.WithStore(approvechat.NewFileStore(dir))
	if !r.l.has("saved state not read") {
		t.Fatal("not logged")
	}
	r.held(t)
}
