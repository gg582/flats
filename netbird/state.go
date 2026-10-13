package netbird

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	accountFile = "account.json"
	configFile  = "netbird.json"     // embed's persisted config (holds the peer private key)
	dnsFile     = "netbird-dns.json" // embed's state file
	portsFile   = "ports.json"

	// All previous releases allocated both roles from 21000-21999. Old
	// public proxies may still forward there after their local records expire.
	legacyPortMin = 21000
	legacyPortMax = 21999

	privatePortMin = 22000
	privatePortMax = 22999
	publicPortMin  = 23000
	publicPortMax  = 23999
)

// Account describes the enrolled NetBird peer. It holds no secrets.
type Account struct {
	// ManagementURL is the NetBird management server ("" is the NetBird
	// default).
	ManagementURL string `json:"management_url"`
	DeviceName    string `json:"device_name"`
	// Fingerprint identifies the peer (a hash of its WireGuard public key).
	Fingerprint string `json:"fingerprint"`
}

// ErrNotConnected is returned by Serve calls while no NetBird peer is enrolled.
var ErrNotConnected = errors.New("netbird: not connected; enroll this host with a setup key first")

// LoadAccount reads the account enrolled in dir. ok is false when none is.
func LoadAccount(dir string) (a Account, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, accountFile))
	if errors.Is(err, os.ErrNotExist) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	if err := json.Unmarshal(b, &a); err != nil || a.Fingerprint == "" {
		return Account{}, false, fmt.Errorf("unreadable %s", filepath.Join(dir, accountFile))
	}
	if _, err := os.Stat(filepath.Join(dir, configFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Account{}, false, nil // account without identity: not usable
		}
		return Account{}, false, err
	}
	return a, true, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), 0o600)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ports maps "private/<host>" and "public/<slug>" to their overlay TCP
// ports. The roles use disjoint ranges, neither of which overlaps the
// historical shared pool. This also protects against stale public routes
// whose old quarantine records have already expired.
//
// "quarantine/<port>" permanently reserves a retired public port, or a
// migrated legacy port, because ending a local NetBird expose session does
// not prove that the remote proxy stopped forwarding. Old quarantine records
// contain expiry timestamps; their values are deliberately ignored.
type ports map[string]int

// portsMu guards ports.json across every Net and RetireStored.
var portsMu sync.Mutex

// RetireStored releases host's recorded ports when no Net is running, for a
// flat deleted while NetBird is off: the private port is freed and the public
// one quarantined, as Retire does. A host with no record is not an error.
func RetireStored(dir, host string) error {
	portsMu.Lock()
	defer portsMu.Unlock()
	p, err := readPorts(dir)
	if err != nil {
		return err
	}
	changed := false
	if _, ok := p[key(rolePrivate, host)]; ok {
		delete(p, key(rolePrivate, host))
		changed = true
	}
	if port, ok := p[key(rolePublic, host)]; ok {
		delete(p, key(rolePublic, host))
		p.quarantine(port)
		changed = true
	}
	if !changed {
		return nil
	}
	return writeJSON(filepath.Join(dir, portsFile), p)
}

const quarantinePrefix = "quarantine/"

// quarantinedPort parses a quarantine key.
func quarantinedPort(key string) (int, bool) {
	s, ok := strings.CutPrefix(key, quarantinePrefix)
	if !ok {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	return v, err == nil
}

// quarantine reserves a port indefinitely. No expiry is safe without proof
// that NetBird deleted every remote forwarding rule for that port.
func (p ports) quarantine(port int) {
	p[quarantinePrefix+strconv.Itoa(port)] = 1
}

func readPorts(dir string) (ports, error) {
	b, err := os.ReadFile(filepath.Join(dir, portsFile))
	if errors.Is(err, os.ErrNotExist) {
		return ports{}, nil
	}
	if err != nil {
		return nil, err
	}
	p := ports{}
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("unreadable %s", filepath.Join(dir, portsFile))
	}
	return p, nil
}

// allocate returns key's port, migrating reservations from the historical
// shared range before returning a listener address.
func (p ports) allocate(k string) (port int, added bool, err error) {
	var start, end int
	switch {
	case strings.HasPrefix(k, rolePrivate+"/"):
		start, end = privatePortMin, privatePortMax
	case strings.HasPrefix(k, rolePublic+"/"):
		start, end = publicPortMin, publicPortMax
	default:
		return 0, false, fmt.Errorf("unknown overlay port role for %q", k)
	}

	if previous, ok := p[k]; ok {
		switch {
		case previous >= start && previous <= end:
			return previous, false, nil
		case previous >= legacyPortMin && previous <= legacyPortMax:
			// An old port may still have an Internet proxy attached.
			// Never bind a current private or public route to it.
			p.quarantine(previous)
			delete(p, k)
		default:
			return 0, false, fmt.Errorf("invalid stored overlay port %d for %q", previous, k)
		}
	}

	used := make(map[int]struct{}, len(p))
	for k, v := range p {
		if q, ok := quarantinedPort(k); ok {
			used[q] = struct{}{}
		} else {
			used[v] = struct{}{}
		}
	}
	for candidate := start; candidate <= end; candidate++ {
		if _, reserved := used[candidate]; reserved {
			continue
		}
		p[k] = candidate
		return candidate, true, nil
	}
	return 0, false, fmt.Errorf("no free overlay port in %d-%d", start, end)
}
