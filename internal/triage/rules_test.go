package triage

import (
	"slices"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		form     string
		items    []string
		found    bool
		want     Level
		wantWhys []string
	}{
		{"earnings", "8-K", []string{"2.02", "9.01"}, true, High,
			[]string{"Item 2.02: earnings results", "Item 9.01: financial statements and exhibits"}},
		{"restatement outranks everything", "8-K", []string{"8.01", "4.02", "5.02"}, true, High,
			[]string{"Item 4.02: past financial statements can't be relied on", "Item 5.02: director or officer change", "Item 8.01: other events"}},
		{"executive change", "8-K", []string{"5.02"}, true, Medium,
			[]string{"Item 5.02: director or officer change"}},
		{"other events only", "8-K", []string{"8.01", "9.01"}, true, Low,
			[]string{"Item 8.01: other events", "Item 9.01: financial statements and exhibits"}},
		{"an item we don't know", "8-K", []string{"6.01"}, true, Low,
			[]string{"Item 6.01: unlisted item"}},
		{"amended 8-K", "8-K/A", []string{"2.02"}, true, High,
			[]string{"Item 2.02: earnings results", "an amendment"}},
		{"8-K whose details SEC didn't give us", "8-K", nil, false, Low,
			[]string{"no item numbers", "item numbers unavailable"}},
		{"annual report", "10-K", nil, true, Medium, []string{"annual report"}},
		{"amended quarterly report", "10-Q/A", nil, true, Medium, []string{"quarterly report", "an amendment"}},
		{"activist stake", "SCHEDULE 13D", nil, true, Medium, []string{"activist stake over 5%"}},
		{"activist stake changed", "SCHEDULE 13D/A", nil, true, Medium, []string{"activist stake changed"}},
		{"passive stake", "SCHEDULE 13G/A", nil, true, Routine, []string{"passive stake over 5%", "an amendment"}},
		{"insider trade", "4", nil, true, Routine, []string{"insider trade"}},
		{"fund paperwork", "485BPOS", nil, true, Routine, []string{"form 485BPOS is not tracked"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.form, tt.items, tt.found)
			if got.Level != tt.want || !slices.Equal(got.Reasons, tt.wantWhys) {
				t.Errorf("Classify(%q, %v) = %s %q, want %s %q", tt.form, tt.items, got.Level, got.Reasons, tt.want, tt.wantWhys)
			}
		})
	}
}

func TestTrack(t *testing.T) {
	for form, want := range map[string]bool{
		"8-K": true, "8-K/A": true, "10-Q": true, "SCHEDULE 13D/A": true, "S-1/A": true,
		"4": false, "4/A": false, "144": false, "SCHEDULE 13G": false, "424B2": false, "": false,
	} {
		if got := Track(form); got != want {
			t.Errorf("Track(%q) = %v, want %v", form, got, want)
		}
	}
}

// Every 8-K item has a level, and the rules table and the levels agree.
func TestEveryItemHasALevel(t *testing.T) {
	for no, r := range items {
		if _, ok := order[r.level]; !ok || r.what == "" {
			t.Errorf("item %s: %+v", no, r)
		}
	}
	if !High.AtLeast(Medium) || Low.AtLeast(Medium) || !Medium.AtLeast(Medium) {
		t.Error("AtLeast orders levels wrongly")
	}
}

func TestRaise(t *testing.T) {
	low := Classify("8-K", []string{"8.01"}, true)
	high := Classify("8-K", []string{"4.02"}, true)
	before := slices.Clone(low.Reasons)

	if got := Raise(low, 85); got.Level != High || got.Reasons[len(got.Reasons)-1] != "raised by the model (score 85)" {
		t.Errorf("8.01 with score 85: %s %q", got.Level, got.Reasons)
	}
	if got := Raise(high, 5); got.Level != High || len(got.Reasons) != len(high.Reasons) {
		t.Errorf("a low score lowered a restatement: %s %q", got.Level, got.Reasons)
	}
	if got := Raise(low, 20); got.Level != Low || len(got.Reasons) != len(low.Reasons) {
		t.Errorf("same level should change nothing: %s %q", got.Level, got.Reasons)
	}
	if !slices.Equal(low.Reasons, before) {
		t.Errorf("Raise changed its input: %q", low.Reasons)
	}
	for score, want := range map[int]Level{0: Routine, 14: Routine, 15: Low, 39: Low, 40: Medium, 69: Medium, 70: High, 100: High} {
		if got := ScoreLevel(score); got != want {
			t.Errorf("ScoreLevel(%d) = %s, want %s", score, got, want)
		}
	}
}
