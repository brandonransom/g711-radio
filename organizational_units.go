package main

import (
	"fmt"
	"regexp"
	"strings"
)

type organizationalUnitType struct {
	name     string
	singular string
	plural   string
	pattern  *regexp.Regexp
}

func unitType(name, singular, plural, pattern string) organizationalUnitType {
	return organizationalUnitType{name, singular, plural, regexp.MustCompile(`(?i)\b(?:` + pattern + `)\b`)}
}

// More specific designations precede the broader types that may also match.
var organizationalUnitTypes = []organizationalUnitType{
	unitType("Refuge Complex", "refuge complex", "refuge complexes", `NWRC|(?:National Wildlife )?Refuge Complex(?:es)?`),
	unitType("National Wildlife Refuge", "wildlife refuge", "wildlife refuges", `NWR|National Wildlife Refuges?`),
	unitType("Wetland Management District", "wetland management district", "wetland management districts", `WMD|Wetland Management Districts?`),
	unitType("Waterfowl Production Area", "waterfowl production area", "waterfowl production areas", `WPA|Waterfowl Production Areas?`),
	unitType("National Fish Hatchery", "fish hatchery", "fish hatcheries", `NFH|National Fish Hatcher(?:y|ies)`),
	unitType("Ecological Services Field Office", "ecological services field office", "ecological services field offices", `FWS ESFO|Ecological Services Field Offices?`),
	unitType("National Historical Park", "historical park", "historical parks", `NHP|National Historical Parks?`),
	unitType("National Historic Site", "historic site", "historic sites", `NHS|National Historic Sites?`),
	unitType("National Battlefield Park", "battlefield park", "battlefield parks", `NBP|National Battlefield Parks?`),
	unitType("National Military Park", "military park", "military parks", `NMP|National Military Parks?`),
	unitType("National Battlefield", "battlefield", "battlefields", `NB|National Battlefields?`),
	unitType("National Memorial", "memorial", "memorials", `NMEM|National Memorials?`),
	unitType("National Preserve", "preserve", "preserves", `NPRES|National Preserves?`),
	unitType("National Reserve", "reserve", "reserves", `NRES|National Reserves?`),
	unitType("National Park", "park", "parks", `NP|National Parks?`),
	unitType("National Monument", "monument", "monuments", `NM|National Monuments?`),
	unitType("National Recreation Area", "recreation area", "recreation areas", `NRA|National Recreation Areas?`),
	unitType("National Seashore", "seashore", "seashores", `NS|National Seashores?`),
	unitType("National Lakeshore", "lakeshore", "lakeshores", `NL|National Lakeshores?`),
	unitType("Parkway", "parkway", "parkways", `PKWY|(?:National )?Parkways?`),
	unitType("Wild and Scenic River", "wild and scenic river", "wild and scenic rivers", `WSR|(?:National )?Wild (?:and|&) Scenic Rivers?`),
	unitType("National Conservation Area", "conservation area", "conservation areas", `NCA|National Conservation Areas?`),
	unitType("BLM District Office", "BLM district office", "BLM district offices", `(?:BLM|Bureau of Land Management)\b.*\b(?:DO|District Offices?)`),
	unitType("BLM Field Office", "BLM field office", "BLM field offices", `(?:BLM|Bureau of Land Management)\b.*\b(?:FO|Field Offices?)`),
	unitType("BLM State Office", "BLM state office", "BLM state offices", `(?:BLM|Bureau of Land Management)\b.*\b(?:SO|State Offices?)`),
	unitType("BIA Regional Office", "BIA regional office", "BIA regional offices", `(?:BIA|Bureau of Indian Affairs)\b.*\b(?:RO|Regional Offices?)`),
	unitType("BIA Agency", "BIA agency", "BIA agencies", `(?:BIA|Bureau of Indian Affairs)\b.*\bAgenc(?:y|ies)`),
	unitType("Reclamation Area Office", "Reclamation area office", "Reclamation area offices", `(?:USBR|BOR|(?:Bureau of )?Reclamation)\b.*\b(?:AO|Area Offices?)`),
	unitType("Reclamation Regional Office", "Reclamation regional office", "Reclamation regional offices", `(?:USBR|BOR|(?:Bureau of )?Reclamation)\b.*\b(?:RO|Regional Offices?)`),
	unitType("Reclamation Project", "Reclamation project", "Reclamation projects", `(?:USBR|BOR|(?:Bureau of )?Reclamation)\b.*\bProjects?`),
	unitType("USGS Water Science Center", "water science center", "water science centers", `(?:USGS|U\.S\. Geological Survey)\b.*\b(?:WSC|Water Science Centers?)`),
	unitType("USGS Science Center", "science center", "science centers", `(?:USGS|U\.S\. Geological Survey)\b.*\b(?:SC|Science Centers?)`),
	unitType("National Forest", "forest", "forests", `NF|National Forests?`),
	unitType("National Grassland", "grassland", "grasslands", `NG|National Grasslands?`),
	unitType("Ranger District", "ranger district", "ranger districts", `RD|Ranger Districts?`),
}

var genericOrganizationalUnit = organizationalUnitType{
	name: "Organizational unit", singular: "organizational unit", plural: "organizational units",
}

func classifyOrganizationalUnit(name string) organizationalUnitType {
	name = strings.Join(strings.Fields(strings.ReplaceAll(name, "_", " ")), " ")
	for _, kind := range organizationalUnitTypes {
		if kind.pattern.MatchString(name) {
			return kind
		}
	}
	return genericOrganizationalUnit
}

// Copy before adding presentation fields; the running inventory is shared
// with reloads and other handlers.
func describeOrganizationalUnits(states []stateGroup) []stateGroup {
	out := make([]stateGroup, len(states))
	for i, state := range states {
		out[i] = state
		out[i].SubGroups = append([]subGroup(nil), state.SubGroups...)
		counts := make(map[string]int)
		var kinds []organizationalUnitType
		for j, group := range state.SubGroups {
			kind := classifyOrganizationalUnit(group.GroupName)
			out[i].SubGroups[j].UnitType = kind.name
			if counts[kind.name] == 0 {
				kinds = append(kinds, kind)
			}
			counts[kind.name]++
		}
		var parts []string
		for _, kind := range kinds {
			count := counts[kind.name]
			label := kind.plural
			if count == 1 {
				label = kind.singular
			}
			parts = append(parts, fmt.Sprintf("%d %s", count, label))
		}
		out[i].UnitSummary = strings.Join(parts, " · ")
	}
	return out
}
