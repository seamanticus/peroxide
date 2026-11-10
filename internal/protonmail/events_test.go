package protonmail

import (
	"slices"
	"testing"
)

// Label IDs built from a set come out sorted, not in map order: with this many
// labels, map order would differ between runs.
// evidence: witnesses
func TestLabelIDsSorted(t *testing.T) {
	current := []string{"e", "a", "d", "b", "c", "f"}

	msg := &Message{LabelIDs: slices.Clone(current)}
	(&EventMessageUpdate{LabelIDsAdded: []string{"h", "g"}, LabelIDsRemoved: []string{"c"}}).Patch(msg)
	if want := []string{"a", "b", "d", "e", "f", "g", "h"}; !slices.Equal(msg.LabelIDs, want) {
		t.Errorf("Patch() LabelIDs = %v, want %v", msg.LabelIDs, want)
	}

	update := &EventMessageUpdate{LabelIDs: []string{"z", "y", "x", "a", "e"}}
	added, removed := update.DiffLabelIDs(current)
	if want := []string{"x", "y", "z"}; !slices.Equal(added, want) {
		t.Errorf("DiffLabelIDs() added = %v, want %v", added, want)
	}
	if want := []string{"b", "c", "d", "f"}; !slices.Equal(removed, want) {
		t.Errorf("DiffLabelIDs() removed = %v, want %v", removed, want)
	}
}
