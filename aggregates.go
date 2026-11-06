package main

import (
	"fmt"
	"sort"
	"strings"
)

// aggregateFilter selects streams for one aggregate sub-page: a stream
// matches when it carries at least one of the tags (case-insensitive).
type aggregateFilter struct {
	Tags []string `json:"tags"`
}

type aggregateGroup struct {
	Name string
	Tags []string
}

// aggregate is a virtual state-level page built from streams anywhere in
// "states" that match its sub-pages' tags. Stream IDs, folders and
// recordings are untouched; the page only references existing streams.
type aggregate struct {
	Name   string
	Groups []aggregateGroup
}

func normalizeTags(tags []string) []string {
	var out []string
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		key := strings.ToLower(tag)
		if tag == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func normalizeAggregates(path string, raw map[string]map[string]aggregateFilter, states []configuredState) ([]aggregate, error) {
	stateNames := make(map[string]struct{}, len(states))
	for _, st := range states {
		stateNames[strings.ToLower(st.StateName)] = struct{}{}
	}

	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	seen := make(map[string]struct{}, len(names))
	out := make([]aggregate, 0, len(names))
	for _, sourceName := range names {
		name := strings.TrimSpace(sourceName)
		if name == "" {
			return nil, fmt.Errorf("%s has an aggregate with an empty name", path)
		}
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s has duplicate aggregate name %q after trimming whitespace", path, name)
		}
		seen[key] = struct{}{}
		if _, clash := stateNames[key]; clash {
			return nil, fmt.Errorf("%s aggregate %q has the same name as a state", path, name)
		}

		rawGroups := raw[sourceName]
		if len(rawGroups) == 0 {
			return nil, fmt.Errorf("%s aggregate %q has no pages configured", path, name)
		}
		groupNames := make([]string, 0, len(rawGroups))
		for g := range rawGroups {
			groupNames = append(groupNames, g)
		}
		sort.Strings(groupNames)

		agg := aggregate{Name: name}
		seenGroups := make(map[string]struct{}, len(groupNames))
		for _, sourceGroup := range groupNames {
			group := strings.TrimSpace(sourceGroup)
			if group == "" {
				return nil, fmt.Errorf("%s aggregate %q has a page with an empty name", path, name)
			}
			if _, dup := seenGroups[strings.ToLower(group)]; dup {
				return nil, fmt.Errorf("%s aggregate %q has duplicate page name %q", path, name, group)
			}
			seenGroups[strings.ToLower(group)] = struct{}{}
			tags := normalizeTags(rawGroups[sourceGroup].Tags)
			if len(tags) == 0 {
				return nil, fmt.Errorf("%s aggregate %q page %q needs at least one tag", path, name, group)
			}
			agg.Groups = append(agg.Groups, aggregateGroup{Name: group, Tags: tags})
		}
		out = append(out, agg)
	}
	return out, nil
}

func hasAnyTag(streamTags, wanted []string) bool {
	for _, w := range wanted {
		for _, t := range streamTags {
			if strings.EqualFold(t, w) {
				return true
			}
		}
	}
	return false
}

// buildAggregateStates returns the virtual states for the /streams listing.
// Sub-pages with no matching streams are omitted, as are empty aggregates.
// A stream matching several sub-pages is listed under each of them.
func buildAggregateStates(aggs []aggregate, states []stateGroup) []stateGroup {
	var out []stateGroup
	for _, agg := range aggs {
		virtual := stateGroup{StateName: agg.Name, Aggregate: true}
		for _, g := range agg.Groups {
			sub := subGroup{GroupName: g.Name, UnitType: "Aggregate"}
			for _, st := range states {
				for _, sg := range st.SubGroups {
					for _, stream := range sg.Streams {
						if hasAnyTag(stream.Tags, g.Tags) {
							sub.Streams = append(sub.Streams, stream)
						}
					}
				}
			}
			if len(sub.Streams) > 0 {
				virtual.SubGroups = append(virtual.SubGroups, sub)
			}
		}
		if len(virtual.SubGroups) == 0 {
			continue
		}
		parts := make([]string, 0, len(virtual.SubGroups))
		for _, sg := range virtual.SubGroups {
			parts = append(parts, fmt.Sprintf("%s (%d)", sg.GroupName, len(sg.Streams)))
		}
		virtual.UnitSummary = strings.Join(parts, " · ")
		out = append(out, virtual)
	}
	return out
}
