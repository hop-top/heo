package desk_test

import (
	"fmt"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

func threadText(th desk.Thread) string {
	var b strings.Builder
	for _, m := range th.Messages {
		b.WriteString(desk.Text(m.Content) + "\n")
	}
	return b.String()
}

// TestThreadFollowsTheCallersAudience: the messages of a conversation a
// helper hands to a model are the caller's own. A desk actor's thread
// holds the internal note when it asks for notes and not when it does
// not; a viewer's never holds it, whatever is asked; and nothing is
// written.
func TestThreadFollowsTheCallersAudience(t *testing.T) {
	w := newWorld(t)
	w.note("INTERNAL-NOTE-TEXT")
	id := string(w.local.Ref.ID)

	deskThread, err := w.as(agent).Thread(t.Context(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	if !deskThread.Notes || !strings.Contains(threadText(deskThread), "INTERNAL-NOTE-TEXT") || !strings.Contains(threadText(deskThread), "hello ada") {
		t.Fatalf("desk thread with notes = %q (notes=%v)", threadText(deskThread), deskThread.Notes)
	}

	plain, err := w.as(agent).Thread(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Notes || strings.Contains(threadText(plain), "INTERNAL-NOTE-TEXT") || plain.Total != deskThread.Total-1 {
		t.Fatalf("desk thread without notes = %q (notes=%v, total %d)", threadText(plain), plain.Notes, plain.Total)
	}

	for _, who := range []struct {
		name  string
		actor model.ActorID
	}{{"viewer", viewer}, {"external", external}} {
		th, err := w.as(who.actor).Thread(t.Context(), id, true)
		if err != nil {
			t.Fatalf("%s thread: %v", who.name, err)
		}
		if th.Notes || strings.Contains(threadText(th), "INTERNAL-NOTE-TEXT") {
			t.Fatalf("%s thread holds the note (notes=%v): %q", who.name, th.Notes, threadText(th))
		}
	}

	// A heo-side policy that lets the caller see everything cannot be
	// told apart from a viewer by uma alone: the audience rule in
	// Service.Sees is the second guard.
	blind := w.as(agent)
	blind.Sees = nil
	th, err := blind.Thread(t.Context(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	if th.Notes || strings.Contains(threadText(th), "INTERNAL-NOTE-TEXT") {
		t.Fatalf("a caller who sees public content only got the note (notes=%v): %q", th.Notes, threadText(th))
	}
}

// TestThreadReadsInRecordedOrderAndKeepsTheNewest: messages come oldest
// first, as uma recorded them, and a thread longer than the window
// loses its oldest messages, never its newest.
func TestThreadReadsInRecordedOrderAndKeepsTheNewest(t *testing.T) {
	w := newWorld(t)
	for i := range 3 {
		w.inject(adapterstest.Inject{Event: fmt.Sprintf("e%d", i), Message: fmt.Sprintf("m%d", i), From: "ada", Destination: "local", Text: fmt.Sprintf("inbound %d", i)})
	}
	id := string(w.local.Ref.ID)
	th, err := w.as(agent).Thread(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(threadText(th)), "\n")
	if len(got) < 4 || got[0] != "hello ada" || got[len(got)-1] != "inbound 2" || th.Cut {
		t.Fatalf("thread = %q cut=%v", got, th.Cut)
	}
}
