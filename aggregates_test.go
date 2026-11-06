package main

import (
	"strings"
	"testing"
)

func TestAggregatePages(t *testing.T) {
	raw := map[string]map[string][]streamConfig{
		"Oregon": {"Umatilla NF": {
			{StreamName: "Admin", UDPPort: 5001},
			{StreamName: "Flight Follow", UDPPort: 5002, Tags: []string{" ff ", "FF", ""}},
			{StreamName: "Guard", UDPPort: 5003, Tags: []string{"AG", "FF"}},
		}},
	}
	states, _, err := normalizeStates("c.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := states[0].SubGroups[0].Streams[1].Tags; len(got) != 1 || got[0] != "ff" {
		t.Fatalf("tags = %q", got)
	}

	aggs, err := normalizeAggregates("c.json", map[string]map[string]aggregateFilter{
		"Aviation": {"All Flight Follow": {Tags: []string{"FF"}}, "All Air Guard": {Tags: []string{"ag"}}, "None": {Tags: []string{"zz"}}},
	}, states)
	if err != nil {
		t.Fatal(err)
	}

	state := stateGroup{StateName: "Oregon", SubGroups: []subGroup{{GroupName: "Umatilla NF"}}}
	for _, sg := range states[0].SubGroups[0].Streams {
		state.SubGroups[0].Streams = append(state.SubGroups[0].Streams, streamInfo{StreamName: sg.StreamName, Tags: sg.Tags})
	}
	out := buildAggregateStates(aggs, []stateGroup{state})
	if len(out) != 1 || !out[0].Aggregate || len(out[0].SubGroups) != 2 {
		t.Fatalf("aggregate states = %+v", out)
	}
	counts := map[string]int{}
	for _, sg := range out[0].SubGroups {
		counts[sg.GroupName] = len(sg.Streams)
	}
	if counts["All Flight Follow"] != 2 || counts["All Air Guard"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestAggregateValidation(t *testing.T) {
	states := []configuredState{{StateName: "Oregon"}}
	cases := map[string]map[string]map[string]aggregateFilter{
		"same name as a state": {"oregon": {"A": {Tags: []string{"x"}}}},
		"needs at least one":   {"Av": {"A": {}}},
		"no pages":             {"Av": {}},
	}
	for want, raw := range cases {
		if _, err := normalizeAggregates("c.json", raw, states); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}
