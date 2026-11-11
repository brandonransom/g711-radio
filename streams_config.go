package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultStreamsReloadInterval = 2 * time.Second

// streamsFileConfig is the layout of a separate streams file (see
// appConfig.StreamsFile): the "states" object (the legacy "regions" spelling
// is still accepted) and the optional "aggregates" pages built from tags.
type streamsFileConfig struct {
	States        map[string]map[string][]streamConfig  `json:"states"`
	LegacyRegions map[string]map[string][]streamConfig  `json:"regions"`
	Aggregates    map[string]map[string]aggregateFilter `json:"aggregates"`
	// Streams is an optional flat list, merged with "states". Each entry
	// names its own state and organizational unit.
	Streams []flatStream `json:"streams"`
	// DispatchCenters maps a dispatch center code to its full name.
	DispatchCenters map[string]string `json:"dispatchCenters"`
}

// flatStream is a stream entry in the flat "streams" list.
type flatStream struct {
	streamConfig
	State string `json:"state"`
	Group string `json:"group"`
}

// mergeFlatStreams folds the flat list into the nested state/group map, so
// everything downstream (IDs, folders, favorites) sees one layout.
func mergeFlatStreams(states map[string]map[string][]streamConfig, flat []flatStream, path string) (map[string]map[string][]streamConfig, error) {
	if len(flat) == 0 {
		return states, nil
	}
	if states == nil {
		states = make(map[string]map[string][]streamConfig)
	}
	for i, fs := range flat {
		state, group := strings.TrimSpace(fs.State), strings.TrimSpace(fs.Group)
		if state == "" || group == "" {
			return nil, fmt.Errorf("%s streams[%d] needs both \"state\" and \"group\"", path, i)
		}
		if states[state] == nil {
			states[state] = make(map[string][]streamConfig)
		}
		states[state][group] = append(states[state][group], fs.streamConfig)
	}
	return states, nil
}

// applyDispatchCenters validates stream tags against the dispatchCenters
// table and fills in each stream's dispatch center name.
func applyDispatchCenters(path string, states []configuredState, table map[string]string) error {
	for si := range states {
		for gi := range states[si].SubGroups {
			streams := states[si].SubGroups[gi].Streams
			for i := range streams {
				s := &streams[i]
				s.GACC = strings.ToUpper(strings.TrimSpace(s.GACC))
				s.DispatchCenter = strings.TrimSpace(s.DispatchCenter)
				if s.DispatchCenter == "" {
					continue
				}
				name, ok := table[s.DispatchCenter]
				if !ok {
					return fmt.Errorf("%s stream %q uses dispatchCenter %q, which is not in \"dispatchCenters\"", path, s.StreamName, s.DispatchCenter)
				}
				s.DispatchCenterName = name
			}
		}
	}
	return nil
}

// streamSet is everything a streams file defines.
type streamSet struct {
	States     []configuredState
	Total      int
	Aggregates []aggregate
}

func (f streamsFileConfig) normalize(path string) (streamSet, error) {
	if len(f.LegacyRegions) > 0 {
		if len(f.States) > 0 {
			return streamSet{}, fmt.Errorf("%s sets both \"states\" and the legacy \"regions\"; use only \"states\"", path)
		}
		f.States = f.LegacyRegions
	}
	merged, err := mergeFlatStreams(f.States, f.Streams, path)
	if err != nil {
		return streamSet{}, err
	}
	states, total, err := normalizeStates(path, merged)
	if err != nil {
		return streamSet{}, err
	}
	if err := applyDispatchCenters(path, states, f.DispatchCenters); err != nil {
		return streamSet{}, err
	}
	aggs, err := normalizeAggregates(path, f.Aggregates, states)
	if err != nil {
		return streamSet{}, err
	}
	return streamSet{States: states, Total: total, Aggregates: aggs}, nil
}

// resolveStreamsPath resolves a relative streamsFile against the folder of
// the config file that names it.
func resolveStreamsPath(configFile, streamsFile string) string {
	streamsFile = strings.TrimSpace(streamsFile)
	if filepath.IsAbs(streamsFile) {
		return streamsFile
	}
	return filepath.Join(filepath.Dir(configFile), streamsFile)
}

func loadStreamsFile(path string) (streamSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return streamSet{}, fmt.Errorf("read streams file: %w", err)
	}
	var f streamsFileConfig
	if err := decodeJSONFile(path, data, &f, true, true); err != nil {
		return streamSet{}, err
	}
	return f.normalize(path)
}

// streamSource says where the stream definitions live, for reloads.
type streamSource struct {
	configFile  string
	streamsPath string
}

func (c appConfig) streamSource() streamSource {
	configFile := c.configFile
	if configFile == "" {
		configFile = configPath
	}
	return streamSource{configFile: configFile, streamsPath: c.streamsPath}
}

func (c appConfig) streamsReloadInterval() time.Duration {
	switch {
	case c.StreamsReloadSeconds < 0:
		return 0
	case c.StreamsReloadSeconds == 0:
		return defaultStreamsReloadInterval
	default:
		return time.Duration(c.StreamsReloadSeconds) * time.Second
	}
}

// streamsFile is the file whose changes add, remove or update streams.
func (src streamSource) streamsFile() string { return src.streamsPath }

func (src streamSource) load() (streamSet, error) { return loadStreamsFile(src.streamsPath) }

// settingsFingerprint hashes the config file's settings, so a reload can
// tell when settings that only take effect on restart were edited.
// Formatting-only edits do not count.
func (src streamSource) settingsFingerprint() (string, error) {
	data, err := os.ReadFile(src.configFile)
	if err != nil {
		return "", err
	}
	var raw map[string]json.RawMessage
	if err := decodeJSONFile(src.configFile, data, &raw, false, true); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw[k]); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%q:%s\n", k, compact.Bytes())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
