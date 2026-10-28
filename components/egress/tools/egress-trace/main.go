// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// egress-trace: read-only view of how the egress sidecar admits outbound
// traffic. Watches the policy API, nftables sets/leases, and /proc sockets
// inside the egress container. Shipped in the image on PATH:
//
//	docker exec -it <egress-container> egress-trace [flags]
//	docker exec -it <egress-container> egress-trace probe <domain>
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	dnsProxyAddr  = "127.0.0.1:15353"
	nftTableName  = "opensandbox"
	authHeader    = "OPENSANDBOX-EGRESS-AUTH"
	tokenEnv      = "OPENSANDBOX_EGRESS_TOKEN"
	httpAddrEnv   = "OPENSANDBOX_EGRESS_HTTP_ADDR"
	probeTimeout  = 3 * time.Second
	policyTimeout = 2 * time.Second
)

const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
)

var useColor = true

func paint(code, s string) string {
	if !useColor || s == "" {
		return s
	}
	return code + s + cReset
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func stampDim() string {
	return paint(cDim, stamp())
}

func main() {
	addr := flag.String("addr", defaultAddr(), "policy API base URL")
	nftBin := flag.String("nft", "nft", "nft binary")
	interval := flag.Duration("interval", time.Second, "poll interval")
	proc := flag.String("proc", "/proc", "proc mount path")
	noColor := flag.Bool("no-color", false, "disable colored output")
	flag.Parse()
	useColor = !*noColor && stdoutIsTTY()

	if flag.NArg() >= 1 && flag.Arg(0) == "probe" {
		if flag.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "usage: egress-trace probe <domain>")
			os.Exit(2)
		}
		os.Exit(runProbe(*addr, flag.Arg(1)))
	}

	runTrace(*addr, *nftBin, *interval, *proc)
}

func defaultAddr() string {
	if v := strings.TrimSpace(os.Getenv(httpAddrEnv)); v != "" {
		if !strings.Contains(v, "://") {
			v = "http://" + v
		}
		return strings.TrimSuffix(v, "/")
	}
	return "http://127.0.0.1:18080"
}

type EgressRule struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

type NetworkPolicy struct {
	DefaultAction string       `json:"defaultAction"`
	Egress        []EgressRule `json:"egress"`
}

type PolicyStatus struct {
	Status          string         `json:"status"`
	Mode            string         `json:"mode"`
	EnforcementMode string         `json:"enforcementMode"`
	Policy          *NetworkPolicy `json:"policy"`
}

func fetchPolicy(addr string) (*PolicyStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), policyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/policy", nil)
	if err != nil {
		return nil, err
	}
	if tok := os.Getenv(tokenEnv); tok != "" {
		req.Header.Set(authHeader, tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var st PolicyStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (p *NetworkPolicy) summary() string {
	if p == nil {
		return "unknown"
	}
	return fmt.Sprintf("default=%s rules=%d", p.DefaultAction, len(p.Egress))
}

func (p *NetworkPolicy) fingerprint() string {
	var b strings.Builder
	for _, r := range p.Egress {
		fmt.Fprintf(&b, "%s:%s;", r.Action, r.Target)
	}
	return b.String()
}

// matchIP returns "" when no static IP/CIDR rule matches (any allow must then
// be DNS-learned).
func (p *NetworkPolicy) matchIP(ip netip.Addr) (string, string) {
	if p == nil {
		return "", ""
	}
	for _, r := range p.Egress {
		if a, err := netip.ParseAddr(r.Target); err == nil {
			if a == ip.Unmap() || a == ip {
				return r.Action, r.Target
			}
			continue
		}
		if pfx, err := netip.ParsePrefix(r.Target); err == nil {
			if pfx.Contains(ip.Unmap()) || pfx.Contains(ip) {
				return r.Action, r.Target
			}
		}
	}
	return "", ""
}

// matchDomain mirrors the sidecar's first-match semantics, falling back to
// defaultAction.
func (p *NetworkPolicy) matchDomain(domain string) (string, string) {
	if p == nil {
		return "deny", "policy API unavailable"
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	for _, r := range p.Egress {
		pattern := strings.ToLower(strings.TrimSpace(r.Target))
		if pattern == "" || strings.Contains(pattern, "/") || mustAddr(pattern).IsValid() {
			continue
		}
		if pattern == domain {
			return orDeny(r.Action), pattern
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:]
			if strings.HasSuffix(domain, suffix) && domain != pattern[2:] {
				return orDeny(r.Action), pattern
			}
		}
	}
	return orDeny(p.DefaultAction), "defaultAction"
}

func mustAddr(s string) netip.Addr {
	a, _ := netip.ParseAddr(s)
	return a
}

func orDeny(a string) string {
	if strings.TrimSpace(a) == "" {
		return "deny"
	}
	return a
}

type nftDoc struct {
	Nftables []map[string]json.RawMessage `json:"nftables"`
}

type nftSet struct {
	Name  string            `json:"name"`
	Table string            `json:"table"`
	Elem  []json.RawMessage `json:"elem"`
}

// intervalFromAny decodes nft interval values: {"prefix":{"addr","len"}} or
// {"range":[from,to]}. Returns "" for anything else.
func intervalFromAny(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if p, ok := m["prefix"].(map[string]any); ok {
		addr, _ := p["addr"].(string)
		length, _ := p["len"].(float64)
		pfx, err := netip.ParsePrefix(addr + "/" + strconv.Itoa(int(length)))
		if err == nil {
			return pfx.String()
		}
		return ""
	}
	if rg, ok := m["range"].([]any); ok && len(rg) == 2 {
		from, _ := rg[0].(string)
		to, _ := rg[1].(string)
		if from != "" && to != "" {
			return from + "-" + to
		}
	}
	return ""
}

type leaseKey struct {
	Set string
	IP  string
}

type leaseChange struct {
	Set     string
	IP      string
	Expires float64
}

type nftWatcher struct {
	bin  string
	sets map[leaseKey]float64
}

func newNftWatcher(bin string) *nftWatcher {
	return &nftWatcher{bin: bin, sets: map[leaseKey]float64{}}
}

func (w *nftWatcher) poll() (added, removed []leaseChange, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), policyTimeout)
	defer cancel()
	out, err := execCommand(ctx, w.bin, "-j", "list", "ruleset")
	if err != nil {
		return nil, nil, err
	}
	var doc nftDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, nil, err
	}
	next := map[leaseKey]float64{}
	addEntry := func(setName, val string, expires float64) {
		if addr, err := netip.ParseAddr(val); err == nil {
			next[leaseKey{setName, addr.String()}] = expires
			return
		}
		// Interval sets render static CIDR (prefix) and range elements here.
		if _, err := netip.ParsePrefix(val); err == nil || strings.Contains(val, "-") {
			next[leaseKey{setName, val}] = expires
		}
	}
	for _, obj := range doc.Nftables {
		var s nftSet
		if json.Unmarshal(obj["set"], &s) != nil || s.Table != nftTableName {
			continue
		}
		for _, raw := range s.Elem {
			var plain string
			if json.Unmarshal(raw, &plain) == nil {
				addEntry(s.Name, plain, 0)
				continue
			}
			// Timeout elements are wrapped in {"elem": ...}; bare interval
			// elements ({"prefix":...}/{"range":[...]}) are not.
			var obj map[string]any
			if json.Unmarshal(raw, &obj) != nil {
				continue
			}
			if inner, ok := obj["elem"].(map[string]any); ok {
				expires, _ := inner["expires"].(float64)
				if expires == 0 {
					expires, _ = inner["timeout"].(float64)
				}
				switch v := inner["val"].(type) {
				case string:
					addEntry(s.Name, v, expires)
				default:
					if cidr := intervalFromAny(v); cidr != "" {
						addEntry(s.Name, cidr, expires)
					}
				}
				continue
			}
			if cidr := intervalFromAny(obj); cidr != "" {
				addEntry(s.Name, cidr, 0)
			}
		}
	}
	for k, expires := range next {
		if _, ok := w.sets[k]; !ok {
			added = append(added, leaseChange{k.Set, k.IP, expires})
		}
	}
	for k := range w.sets {
		if _, ok := next[k]; !ok {
			removed = append(removed, leaseChange{k.Set, k.IP, 0})
		}
	}
	sortChanges(added)
	sortChanges(removed)
	w.sets = next
	return added, removed, nil
}

func sortChanges(changes []leaseChange) {
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Set != changes[j].Set {
			return changes[i].Set < changes[j].Set
		}
		return changes[i].IP < changes[j].IP
	})
}

func (w *nftWatcher) hasIP(ip string) (bool, float64) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false, 0
	}
	for k, expires := range w.sets {
		if strings.HasPrefix(k.Set, "dyn_allow") && k.IP == addr.String() {
			return true, expires
		}
	}
	return false, 0
}

func execCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "),
			err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

type flowKey struct {
	IP   string
	Port string
}

type flowWatcher struct {
	proc  string
	flows map[flowKey]string
}

func newFlowWatcher(proc string) *flowWatcher {
	return &flowWatcher{proc: proc, flows: map[flowKey]string{}}
}

func (w *flowWatcher) poll() (established, closed []flowKey, err error) {
	current := map[flowKey]string{}
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		data, err := os.ReadFile(w.proc + "/" + name)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, err
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			_, lport, ok := parseProcAddr(fields[1])
			if !ok || isEgressListener(lport) {
				continue
			}
			ip, port, ok := parseProcAddr(fields[2])
			if !ok || isLoopback(ip) {
				continue
			}
			st := fields[3]
			if st == "01" || st == "02" {
				current[flowKey{ip, port}] = st
			}
		}
	}
	for k, st := range current {
		old, seen := w.flows[k]
		if st == "02" && !seen {
			continue
		}
		if st == "01" && (!seen || old != "01") {
			established = append(established, k)
		}
	}
	for k := range w.flows {
		if _, ok := current[k]; !ok {
			closed = append(closed, k)
		}
	}
	w.flows = current
	return established, closed, nil
}

// parseProcAddr decodes "A1B2C3D4:01BB"; the kernel prints little-endian
// 32-bit words, so bytes reverse within each 4-byte group.
func parseProcAddr(field string) (ip, port string, ok bool) {
	host, portHex, found := strings.Cut(field, ":")
	if !found || (len(host) != 8 && len(host) != 32) {
		return "", "", false
	}
	raw, err := hex.DecodeString(host)
	if err != nil {
		return "", "", false
	}
	for group := 0; group < len(raw); group += 4 {
		raw[group], raw[group+3] = raw[group+3], raw[group]
		raw[group+1], raw[group+2] = raw[group+2], raw[group+1]
	}
	parsed := net.IP(raw)
	if len(host) == 8 {
		if v4 := parsed.To4(); v4 != nil {
			parsed = v4
		}
	}
	p, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return "", "", false
	}
	return parsed.String(), strconv.Itoa(int(p)), true
}

func isLoopback(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	return err != nil || addr.IsLoopback() || addr.IsUnspecified()
}

// isEgressListener drops inbound legs: accepted sockets on the sidecar's own
// listeners have the listener port as the LOCAL port.
func isEgressListener(port string) bool {
	return port == "18080" || port == "15353"
}

func runTrace(addr, nftBin string, interval time.Duration, proc string) {
	bar := paint(cDim, "─────────────────────────────────────────────────────────────")
	fmt.Println(bar)
	fmt.Printf(" %s %s\n", paint(cBold, "egress-trace"),
		paint(cDim, "read-only watch of this egress namespace (Ctrl-C to stop)"))
	fmt.Printf(" %s %s\n", paint(cDim, fmt.Sprintf("%-10s", "policy")), addr+"/policy")
	fmt.Printf(" %s %s %s\n", paint(cDim, fmt.Sprintf("%-10s", "nft table")), nftTableName,
		paint(cDim, "(static sets + DNS-learned leases)"))
	fmt.Printf(" %s %s\n", paint(cDim, fmt.Sprintf("%-10s", "sockets")), proc+"/net/tcp{,6}")
	fmt.Printf(" %s %s\n", paint(cDim, fmt.Sprintf("%-10s", "note")),
		paint(cDim, "L7 request contents are never logged (by design)"))
	fmt.Println(bar)

	var pol *NetworkPolicy
	var lastSummary string

	nftw := newNftWatcher(nftBin)
	floww := newFlowWatcher(proc)

	// First snapshot is the baseline: existing sets are state, not events.
	if _, _, err := nftw.poll(); err != nil {
		warnf("nft: %v", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	tick := 0
	for {
		if st, err := fetchPolicy(addr); err != nil {
			if tick%10 == 0 {
				warnf("policy API: %v", err)
			}
		} else {
			pol = st.Policy
			sum := st.Mode + "|" + pol.fingerprint()
			if sum != lastSummary {
				fmt.Printf("%s %s %s %s\n", stampDim(), paint(cBold, "POLICY"),
					paint(cBold, "mode="+st.Mode),
					paint(cDim, pol.summary()))
				for _, r := range pol.Egress {
					color := cGreen
					if orDeny(r.Action) == "deny" {
						color = cRed
					}
					fmt.Printf("         %s %s\n", paint(color, fmt.Sprintf("%-5s", orDeny(r.Action))),
						paint(cBold, r.Target))
				}
				lastSummary = sum
			}
		}

		if added, removed, err := nftw.poll(); err != nil {
			if tick%10 == 0 {
				warnf("nft: %v", err)
			}
		} else {
			for _, lc := range added {
				src, srcColor := "dns-learned", cCyan
				if action, target := pol.matchIP(mustAddr(lc.IP)); action != "" {
					src = fmt.Sprintf("static rule %s %s", action, target)
					if action == "deny" {
						srcColor = cRed
					} else {
						srcColor = cGreen
					}
				} else if strings.Contains(lc.IP, "/") || strings.Contains(lc.IP, "-") {
					src, srcColor = "static CIDR rule", cGreen
				}
				ttl := ""
				if lc.Expires > 0 {
					ttl = fmt.Sprintf(" ttl %ds", int(lc.Expires))
				}
				fmt.Printf("%s %s %s %s %s%s\n",
					stampDim(),
					paint(cBold+cGreen, "LEASE+"),
					paint(cBold, fmt.Sprintf("%-15s", lc.IP)),
					paint(cDim, "set="+lc.Set),
					paint(srcColor, src),
					paint(cYellow, ttl))
			}
			for _, lc := range removed {
				fmt.Printf("%s %s %s %s\n",
					stampDim(),
					paint(cDim, "LEASE-"),
					paint(cDim, fmt.Sprintf("%-15s", lc.IP)),
					paint(cDim, "set="+lc.Set))
			}
		}

		if established, closed, err := floww.poll(); err != nil {
			if tick%10 == 0 {
				warnf("proc: %v", err)
			}
		} else {
			for _, f := range established {
				note, noteColor := "", cDim
				if ok, expires := nftw.hasIP(f.IP); ok {
					note, noteColor = fmt.Sprintf("admitted via lease (ttl %ds)", int(expires)), cGreen
				} else if action, target := pol.matchIP(mustAddr(f.IP)); action == "allow" {
					note, noteColor = "static allow "+target, cGreen
				} else if action == "deny" {
					note, noteColor = "UNEXPECTED: static deny target connected", cRed+cBold
				} else if pol != nil && orDeny(pol.DefaultAction) == "allow" {
					note, noteColor = "default allow", cGreen
				}
				fmt.Printf("%s %s %s %s %s\n",
					stampDim(),
					paint(cBold+cCyan, "FLOW  "),
					paint(cBold, f.IP+":"+f.Port),
					paint(cGreen, "ESTABLISHED"),
					paint(noteColor, note))
			}
			for _, f := range closed {
				fmt.Printf("%s %s %s %s\n",
					stampDim(),
					paint(cDim, "FLOW  "),
					paint(cDim, f.IP+":"+f.Port),
					paint(cDim, "closed"))
			}
		}

		select {
		case <-stop:
			return
		case <-ticker.C:
			tick++
		}
	}
}

func stamp() string {
	return time.Now().Format("15:04:05.000")
}

func warnf(format string, args ...any) {
	fmt.Printf("%s %s "+format+"\n", append([]any{stampDim(), paint(cYellow, "WARN ")}, args...)...)
}

func runProbe(addr, domain string) int {
	st, err := fetchPolicy(addr)
	pol := (*NetworkPolicy)(nil)
	if err != nil {
		fmt.Printf("policy API unavailable (%v); verdict unknown\n", err)
	} else {
		pol = st.Policy
	}
	action, matched := pol.matchDomain(domain)
	verdictColor := cRed
	if action == "allow" {
		verdictColor = cGreen
	}
	fmt.Printf("policy verdict : %s (matched: %s)\n",
		paint(cBold+verdictColor, strings.ToUpper(action)),
		paint(cBold, matched))

	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, dnsProxyAddr)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	addrs, err := r.LookupHost(ctx, domain)
	switch {
	case err == nil:
		fmt.Printf("dns resolution : %s\n", strings.Join(addrs, ", "))
		if st != nil && st.EnforcementMode == "dns" {
			fmt.Println("outcome        : " + paint(cGreen, "resolved and allowed; DNS-only enforcement — admission is decided by name, there is no packet layer"))
		} else {
			fmt.Println("outcome        : " + paint(cGreen, "resolved; expect an nft lease for these IPs and an admitted dial"))
		}
	case isNXDOMAIN(err):
		fmt.Println("dns resolution : NXDOMAIN")
		if action == "deny" {
			fmt.Println("outcome        : " + paint(cRed, "denied by policy at the DNS layer (no network attempt will happen)"))
		} else {
			fmt.Println("outcome        : " + paint(cYellow, "not resolved — either the name does not exist upstream, or a platform overlay (deny.always) denies it; GET /policy does not include overlays"))
		}
	default:
		fmt.Printf("dns resolution : error (%v)\n", err)
		fmt.Println("outcome        : " + paint(cRed, "resolver/upstream failure — distinct from a policy denial"))
	}
	return 0
}

func isNXDOMAIN(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}
