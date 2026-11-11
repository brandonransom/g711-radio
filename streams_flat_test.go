package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadStreamsFileFlatAndDispatchCenters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "streams.json")
	body := `{
  "states": {"Utah": {"Ashley NF": [{"streamName": "A", "udpPort": 41001}]}},
  "streams": [
    {"state": "Utah", "group": "Ashley NF", "streamName": "B", "udpPort": 41002, "gacc": "gbcc", "dispatchCenter": "VDC"},
    {"state": "Oregon", "group": "Umatilla NF", "streamName": "C", "udpPort": 41003, "gacc": "NWCC", "dispatchCenter": "PDC"}
  ],
  "dispatchCenters": {"VDC": "Vernal Dispatch Center", "PDC": "Pendleton Dispatch Center"}
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := loadStreamsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if set.Total != 3 || len(set.States) != 2 {
		t.Fatalf("total=%d states=%d, want 3 and 2", set.Total, len(set.States))
	}
	var b streamConfig
	for _, st := range set.States {
		for _, sg := range st.SubGroups {
			for _, s := range sg.Streams {
				if s.StreamName == "B" {
					b = s
				}
			}
		}
	}
	if b.GACC != "GBCC" || b.DispatchCenterName != "Vernal Dispatch Center" {
		t.Fatalf("got gacc=%q name=%q", b.GACC, b.DispatchCenterName)
	}

	bad := `{"streams": [{"state": "Utah", "group": "G", "streamName": "X", "udpPort": 41009, "dispatchCenter": "NOPE"}]}`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStreamsFile(path); err == nil {
		t.Fatal("expected error for unknown dispatchCenter")
	}
}
