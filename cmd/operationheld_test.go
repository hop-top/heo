package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"hop.top/kit/go/console/output"
)

// TestHeldFailureReportsPartialBatch: a batch that decided some and not
// others names the failures and exits generic; every result stays in the
// output; a single failure exits as its own error would.
func TestHeldFailureReportsPartialBatch(t *testing.T) {
	ok := heldResult{Operation: "op_1", OK: true, Status: "queued"}
	bad := heldResult{Operation: "op_2", Error: "stale: x", ErrorCode: output.CodeConflict}
	if err := heldFailure(heldDecideOut{Results: []heldResult{ok}}); err != nil {
		t.Fatalf("all decided: %v", err)
	}
	err := heldFailure(heldDecideOut{Results: []heldResult{ok, bad, {Operation: "op_3", Error: "gone"}}, Failed: 2})
	if err == nil || !strings.Contains(err.Error(), "2 of 3") || !strings.Contains(err.Error(), "op_2, op_3") || strings.Contains(err.Error(), "op_1") {
		t.Fatalf("partial = %v", err)
	}
	one := heldFailure(heldDecideOut{Results: []heldResult{bad}, Failed: 1})
	oe, isOut := one.(*output.Error)
	if !isOut || oe.Code != output.CodeConflict || oe.ExitCode != output.ExitConflict {
		t.Fatalf("single = %#v", one)
	}
	rows := heldDecideRows(heldDecideOut{Decision: "approve", Results: []heldResult{ok, bad}, OperationKey: "k"})
	if rows[1][1] != "queued" || rows[2][1] != "failed: stale: x" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestHeldIDsNamesTheOperationOneWay(t *testing.T) {
	cmd := func(flagged ...string) *cobra.Command {
		c := &cobra.Command{Use: "x"}
		c.Flags().StringSlice(flagOperation, nil, "")
		for _, f := range flagged {
			_ = c.Flags().Set(flagOperation, f)
		}
		return c
	}
	if ids, err := heldIDs(cmd(), []string{"op_1", "op_2"}); err != nil || len(ids) != 2 {
		t.Errorf("positional = %v, %v", ids, err)
	}
	if ids, err := heldIDs(cmd("op_1", "op_2"), nil); err != nil || len(ids) != 2 || ids[1] != "op_2" {
		t.Errorf("--operation = %v, %v", ids, err)
	}
	if _, err := heldIDs(cmd(), nil); err == nil {
		t.Error("neither form named an operation: want a usage error")
	}
	if _, err := heldIDs(cmd("op_1"), []string{"op_2"}); err == nil {
		t.Error("both forms named an operation: want a usage error")
	}
	s := &cobra.Command{Use: "x"}
	s.Flags().String(flagOperation, "", "")
	_ = s.Flags().Set(flagOperation, "op_1")
	if ids, err := heldIDs(s, nil); err != nil || len(ids) != 1 || ids[0] != "op_1" {
		t.Errorf("string flag = %v, %v", ids, err)
	}
}

func approveFlags(set map[string]string, ops ...string) *cobra.Command {
	c := &cobra.Command{Use: "approve"}
	c.Flags().StringSlice(flagOperation, nil, "")
	addSelectorFlags(c)
	for k, v := range set {
		_ = c.Flags().Set(k, v)
	}
	for _, o := range ops {
		_ = c.Flags().Set(flagOperation, o)
	}
	return c
}

func TestHeldSelectorNeedsClassAndMailbox(t *testing.T) {
	// No selector flag: the operations are named.
	if _, _, picked, err := heldSelectorOf(approveFlags(nil), []string{"op_1"}); picked || err != nil {
		t.Fatalf("named = picked %v, %v", picked, err)
	}
	// Reject has no selector flags at all.
	if _, _, picked, err := heldSelectorOf(&cobra.Command{Use: "reject"}, nil); picked || err != nil {
		t.Fatalf("reject = picked %v, %v", picked, err)
	}
	// A partial selector is refused, never read as "everything".
	for name, set := range map[string]map[string]string{
		"class only": {flagClass: "trash"}, "inbox only": {flagInbox: "support"},
		"proposer only": {flagProposer: "plugin:triage"}, "max only": {flagMax: "5"},
		"blank class": {flagClass: "  ", flagInbox: "support"},
	} {
		if _, _, _, err := heldSelectorOf(approveFlags(set), nil); err == nil {
			t.Errorf("%s: want a usage error", name)
		}
	}
	sel, max, picked, err := heldSelectorOf(approveFlags(map[string]string{flagClass: "trash", flagInbox: "support", flagProposer: "plugin:triage", flagMax: "7"}), nil)
	if err != nil || !picked || sel.Class != "trash" || sel.Inbox != "support" || sel.Proposer != "plugin:triage" || max != 7 {
		t.Fatalf("selector = %+v, %d, %v, %v", sel, max, picked, err)
	}
	// A selector and named operations are two ways to name them.
	set := map[string]string{flagClass: "trash", flagInbox: "support"}
	if _, _, _, err := heldSelectorOf(approveFlags(set), []string{"op_1"}); err == nil {
		t.Error("selector with a positional operation: want a usage error")
	}
	if _, _, _, err := heldSelectorOf(approveFlags(set, "op_1"), nil); err == nil {
		t.Error("selector with --operation: want a usage error")
	}
}

func TestParseEditArgs(t *testing.T) {
	// `held show --format json` output with its intent edited names the
	// operation and revision it was read at.
	got, err := parseEditArgs(`{"id":"op_1","revision":"r9","kind":"message.edit","intent":{"content":{"x":1}}}`)
	if err != nil || got.Operation != "op_1" || got.Revision != "r9" || string(got.Intent) != `{"content":{"x":1}}` {
		t.Fatalf("show document = %+v, %v", got, err)
	}
	// A document with no intent member is the intent itself.
	got, err = parseEditArgs(`{"content":{"x":1}}`)
	if err != nil || got.Operation != "" || got.Revision != "" || string(got.Intent) != `{"content":{"x":1}}` {
		t.Fatalf("bare intent = %+v, %v", got, err)
	}
	for name, bad := range map[string]string{
		"empty": ``, "not json": `{nope`, "array": `[1]`, "null": `null`, "two": `{"a":1} {"b":2}`,
		"intent not an object": `{"intent":"x"}`, "revision not a string": `{"revision":5,"intent":{}}`,
	} {
		if _, err := parseEditArgs(bad); err == nil {
			t.Errorf("%s: want a usage error", name)
		}
	}
}
