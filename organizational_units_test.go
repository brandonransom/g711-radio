package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrganizationalUnitTypes(t *testing.T) {
	cases := []struct {
		acronym, fullName, want string
	}{
		{"NP", "National Park", "National Park"},
		{"NPRES", "National Preserve", "National Preserve"},
		{"NM", "National Monument", "National Monument"},
		{"NRA", "National Recreation Area", "National Recreation Area"},
		{"NWR", "National Wildlife Refuge", "National Wildlife Refuge"},
		{"NWRC", "National Wildlife Refuge Complex", "Refuge Complex"},
		{"WMD", "Wetland Management District", "Wetland Management District"},
		{"NFH", "National Fish Hatchery", "National Fish Hatchery"},
		{"BLM Test DO", "Bureau of Land Management Test District Office", "BLM District Office"},
		{"BLM Test FO", "Bureau of Land Management Test Field Office", "BLM Field Office"},
		{"BLM Test SO", "Bureau of Land Management Test State Office", "BLM State Office"},
		{"BIA Test Agency", "Bureau of Indian Affairs Test Agency", "BIA Agency"},
		{"USBR Test AO", "Bureau of Reclamation Test Area Office", "Reclamation Area Office"},
		{"USBR Test Project", "Bureau of Reclamation Test Project", "Reclamation Project"},
		{"NHP", "National Historical Park", "National Historical Park"},
		{"NHS", "National Historic Site", "National Historic Site"},
		{"NMEM", "National Memorial", "National Memorial"},
		{"NB", "National Battlefield", "National Battlefield"},
		{"NBP", "National Battlefield Park", "National Battlefield Park"},
		{"NMP", "National Military Park", "National Military Park"},
		{"NS", "National Seashore", "National Seashore"},
		{"NL", "National Lakeshore", "National Lakeshore"},
		{"PKWY", "Parkway", "Parkway"},
		{"NRES", "National Reserve", "National Reserve"},
		{"WSR", "Wild and Scenic River", "Wild and Scenic River"},
		{"WPA", "Waterfowl Production Area", "Waterfowl Production Area"},
		{"FWS ESFO", "Ecological Services Field Office", "Ecological Services Field Office"},
		{"BIA Test RO", "Bureau of Indian Affairs Test Regional Office", "BIA Regional Office"},
		{"USBR Test RO", "Bureau of Reclamation Test Regional Office", "Reclamation Regional Office"},
		{"USGS Test SC", "U.S. Geological Survey Test Science Center", "USGS Science Center"},
		{"USGS Test WSC", "U.S. Geological Survey Test Water Science Center", "USGS Water Science Center"},
		{"NCA", "National Conservation Area", "National Conservation Area"},
		{"NF", "National Forest", "National Forest"},
		{"NG", "National Grassland", "National Grassland"},
		{"RD", "Ranger District", "Ranger District"},
	}
	for _, tc := range cases {
		for _, designation := range []string{tc.acronym, tc.fullName} {
			for _, name := range []string{"Test " + designation, strings.ToLower(designation) + " - Primary"} {
				t.Run(name, func(t *testing.T) {
					if got := classifyOrganizationalUnit(name).name; got != tc.want {
						t.Fatalf("got %q, want %q", got, tc.want)
					}
				})
			}
		}
	}
	for _, name := range []string{"NFF", "Radio Test", "Wolf Creek", "NPRESERVE", "Field Office", "Tribal Government"} {
		if got := classifyOrganizationalUnit(name).name; got != "Organizational unit" {
			t.Errorf("%q classified as %q", name, got)
		}
	}
}

func TestOrganizationalUnitSummaryAPI(t *testing.T) {
	state := stateGroup{StateName: "Oregon"}
	for i := 0; i < 6; i++ {
		state.SubGroups = append(state.SubGroups, subGroup{GroupName: "Test NF", Streams: make([]streamInfo, 4)})
	}
	for i := 0; i < 4; i++ {
		state.SubGroups = append(state.SubGroups, subGroup{GroupName: "Test National Wildlife Refuge", Streams: make([]streamInfo, 4)})
	}
	server := &webrtcServer{stateGroups: []stateGroup{state}}
	response := httptest.NewRecorder()
	server.handleStreams(response, httptest.NewRequest(http.MethodGet, "/streams", nil))
	var got []stateGroup
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UnitSummary != "6 forests · 4 wildlife refuges" {
		t.Fatalf("unexpected response: %s", response.Body)
	}
	streams := 0
	for _, group := range got[0].SubGroups {
		streams += len(group.Streams)
	}
	if streams != 40 || got[0].SubGroups[6].UnitType != "National Wildlife Refuge" {
		t.Fatalf("unexpected response: %s", response.Body)
	}
	if state.SubGroups[0].UnitType != "" || server.stateGroups[0].UnitSummary != "" {
		t.Fatal("presentation fields mutated shared inventory")
	}
}

func TestOrganizationalUnitSummaryPlurals(t *testing.T) {
	got := describeOrganizationalUnits([]stateGroup{{SubGroups: []subGroup{
		{GroupName: "Umatilla NF"}, {GroupName: "Test NWR"}, {GroupName: "Test NFH"},
		{GroupName: "First NWRC"}, {GroupName: "Second NWRC"}, {GroupName: "Radio Test"},
	}}})[0].UnitSummary
	want := "1 forest · 1 wildlife refuge · 1 fish hatchery · 2 refuge complexes · 1 organizational unit"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
