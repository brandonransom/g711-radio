package main

import (
	"bufio"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

// geoInfo is the location attached to a truncated network.
type geoInfo struct {
	Override string // label from the overrides file, if any
	City     string
	Region   string
	Country  string // ISO code
}

// Label is the best available description: the override if set, otherwise
// "City, Region, CC" from the GeoIP database.
func (g geoInfo) Label() string {
	if g.Override != "" {
		return g.Override
	}
	parts := make([]string, 0, 3)
	for _, p := range []string{g.City, g.Region, g.Country} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

type locationOverride struct {
	prefix netip.Prefix
	label  string
}

// geoLocator resolves networks to locations. Both sources are optional. The
// overrides file is re-read whenever it changes, so edits apply without a
// restart.
type geoLocator struct {
	logger        *log.Logger
	db            *maxminddb.Reader
	overridesPath string

	mu          sync.Mutex
	cache       map[string]geoInfo
	overrides   []locationOverride
	overridesAt time.Time
}

func newGeoLocator(dbPath, overridesPath string, logger *log.Logger) *geoLocator {
	g := &geoLocator{logger: logger, overridesPath: overridesPath, cache: map[string]geoInfo{}}
	if dbPath != "" {
		db, err := maxminddb.Open(dbPath)
		if err != nil {
			logger.Printf("analytics: GeoIP database unavailable (%v); locations will use overrides only", err)
		} else {
			g.db = db
			logger.Printf("analytics: GeoIP database %s (%s, built %s)", dbPath, db.Metadata.DatabaseType,
				time.Unix(int64(db.Metadata.BuildEpoch), 0).Format(dateLayout))
		}
	}
	return g
}

func (g *geoLocator) Close() {
	if g != nil && g.db != nil {
		_ = g.db.Close()
	}
}

// Source describes the configured location sources for the dashboard.
func (g *geoLocator) Source() string {
	var s []string
	if g.db != nil {
		s = append(s, g.db.Metadata.DatabaseType+" built "+time.Unix(int64(g.db.Metadata.BuildEpoch), 0).Format(dateLayout))
	}
	if g.overridesPath != "" {
		s = append(s, "overrides file "+g.overridesPath)
	}
	if len(s) == 0 {
		return "none configured (raw networks only)"
	}
	return strings.Join(s, "; ")
}

func (g *geoLocator) refreshOverridesLocked() {
	if g.overridesPath == "" {
		return
	}
	st, err := os.Stat(g.overridesPath)
	if err != nil {
		if g.overrides != nil {
			g.overrides, g.overridesAt = nil, time.Time{}
			g.cache = map[string]geoInfo{}
		}
		return
	}
	if st.ModTime().Equal(g.overridesAt) {
		return
	}
	f, err := os.Open(g.overridesPath)
	if err != nil {
		return
	}
	defer f.Close()
	var list []locationOverride
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		cidr, label, ok := strings.Cut(text, ",")
		if !ok {
			g.logger.Printf("analytics: %s line %d: expected \"cidr,label\"", g.overridesPath, line)
			continue
		}
		p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			g.logger.Printf("analytics: %s line %d: %v", g.overridesPath, line, err)
			continue
		}
		list = append(list, locationOverride{prefix: p.Masked(), label: strings.Trim(strings.TrimSpace(label), `"`)})
	}
	g.overrides, g.overridesAt = list, st.ModTime()
	g.cache = map[string]geoInfo{}
}

type mmdbCity struct {
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Subdivisions []struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"subdivisions"`
	Country struct {
		IsoCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

// Lookup locates a truncated network such as "199.131.68.0/24".
func (g *geoLocator) Lookup(network string) geoInfo {
	if g == nil {
		return geoInfo{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refreshOverridesLocked()
	if info, ok := g.cache[network]; ok {
		return info
	}
	var info geoInfo
	p, err := netip.ParsePrefix(network)
	if err == nil {
		addr := p.Addr()
		best := -1
		for _, o := range g.overrides {
			if o.prefix.Bits() > best && o.prefix.Contains(addr) {
				best, info.Override = o.prefix.Bits(), o.label
			}
		}
		if g.db != nil {
			var rec mmdbCity
			if g.db.Lookup(net.IP(addr.AsSlice()), &rec) == nil {
				info.City = rec.City.Names["en"]
				if len(rec.Subdivisions) > 0 {
					info.Region = rec.Subdivisions[0].Names["en"]
				}
				info.Country = rec.Country.IsoCode
			}
		}
	}
	g.cache[network] = info
	return info
}
