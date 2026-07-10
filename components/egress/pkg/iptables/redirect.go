// Copyright 2026 Alibaba Group Holding Ltd.
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

package iptables

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

const dnsRedirectNftTable = "opensandbox_dns_redirect"

var dnsRedirectNftFamilies = []string{"inet", "ip", "ip6"}

type nftCleanupError struct {
	errs []error
}

func (e nftCleanupError) Error() string {
	msgs := make([]string, 0, len(e.errs))
	for _, err := range e.errs {
		msgs = append(msgs, err.Error())
	}
	return "nft DNS redirect cleanup failed: " + strings.Join(msgs, "; ")
}

type commandRunner func(ctx context.Context, args []string) ([]byte, error)

type nftRunner func(ctx context.Context, script string) ([]byte, error)

type redirectRunner struct {
	runCommand commandRunner
	runNft     nftRunner
}

func defaultRedirectRunner() redirectRunner {
	return redirectRunner{
		runCommand: func(ctx context.Context, args []string) ([]byte, error) {
			return exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		},
		runNft: func(ctx context.Context, script string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "nft", "-f", "-")
			cmd.Stdin = strings.NewReader(script)
			return cmd.CombinedOutput()
		},
	}
}

func dnsRedirectRules(port int, exemptDst []netip.Addr, op string) [][]string {
	targetPort := strconv.Itoa(port)

	var rules [][]string
	for _, d := range exemptDst {
		addr := d
		dStr := d.String()
		if addr.Is4() {
			rules = append(rules,
				[]string{"iptables", "-t", "nat", op, "OUTPUT", "-p", "udp", "--dport", "53", "-d", dStr, "-j", "RETURN"},
				[]string{"iptables", "-t", "nat", op, "OUTPUT", "-p", "tcp", "--dport", "53", "-d", dStr, "-j", "RETURN"},
			)
		} else {
			rules = append(rules,
				[]string{"ip6tables", "-t", "nat", op, "OUTPUT", "-p", "udp", "--dport", "53", "-d", dStr, "-j", "RETURN"},
				[]string{"ip6tables", "-t", "nat", op, "OUTPUT", "-p", "tcp", "--dport", "53", "-d", dStr, "-j", "RETURN"},
			)
		}
	}
	markAndRedirect := [][]string{
		{"iptables", "-t", "nat", op, "OUTPUT", "-p", "udp", "--dport", "53", "-m", "mark", "--mark", constants.MarkHex, "-j", "RETURN"},
		{"iptables", "-t", "nat", op, "OUTPUT", "-p", "tcp", "--dport", "53", "-m", "mark", "--mark", constants.MarkHex, "-j", "RETURN"},
		{"iptables", "-t", "nat", op, "OUTPUT", "-p", "udp", "--dport", "53", "-j", "REDIRECT", "--to-port", targetPort},
		{"iptables", "-t", "nat", op, "OUTPUT", "-p", "tcp", "--dport", "53", "-j", "REDIRECT", "--to-port", targetPort},
		{"ip6tables", "-t", "nat", op, "OUTPUT", "-p", "udp", "--dport", "53", "-m", "mark", "--mark", constants.MarkHex, "-j", "RETURN"},
		{"ip6tables", "-t", "nat", op, "OUTPUT", "-p", "tcp", "--dport", "53", "-m", "mark", "--mark", constants.MarkHex, "-j", "RETURN"},
		{"ip6tables", "-t", "nat", op, "OUTPUT", "-p", "udp", "--dport", "53", "-j", "REDIRECT", "--to-port", targetPort},
		{"ip6tables", "-t", "nat", op, "OUTPUT", "-p", "tcp", "--dport", "53", "-j", "REDIRECT", "--to-port", targetPort},
	}
	rules = append(rules, markAndRedirect...)
	return rules
}

func (r redirectRunner) runRedirectRules(ctx context.Context, rules [][]string) ([][]string, error) {
	var applied [][]string
	for _, args := range rules {
		if output, err := r.runCommand(ctx, args); err != nil {
			return applied, fmt.Errorf("iptables command failed: %v (output: %s)", err, output)
		}
		applied = append(applied, args)
	}
	return applied, nil
}

func (r redirectRunner) setupRedirect(ctx context.Context, port int, exemptDst []netip.Addr) error {
	if cleanupErr := r.removeNftRedirectTables(ctx); cleanupErr != nil {
		if !isNftUnavailableError(cleanupErr) {
			return cleanupErr
		}
		log.Warnf("nft DNS redirect cleanup unavailable before iptables setup (ignored): %v", cleanupErr)
	}
	rules := dnsRedirectRules(port, exemptDst, "-A")
	if applied, err := r.runRedirectRules(ctx, rules); err != nil {
		if !isIptablesNftOutputAppendMissingChain(err) {
			r.rollbackRedirectRules(ctx, applied)
			return err
		}
		log.Warnf("iptables DNS redirect failed in nft backend; falling back to native nft redirect: %v", err)
		if rollbackErr := r.rollbackRedirectRules(ctx, applied); rollbackErr != nil {
			return fmt.Errorf("iptables rollback failed before nft DNS redirect fallback after iptables error %v: %w", err, rollbackErr)
		}
		script := dnsRedirectNftScript(port, exemptDst)
		if output, nftErr := r.runNft(ctx, script); nftErr != nil {
			if isNftMissingTableError(output, nftErr) {
				if retryOutput, retryErr := r.runNft(ctx, removeNftDeleteTableLine(script)); retryErr == nil {
					log.Infof("nft DNS redirect fallback installed successfully after missing-table retry")
					return nil
				} else {
					return fmt.Errorf("nft DNS redirect fallback retry failed after iptables error %v: %w (output: %s)", err, retryErr, strings.TrimSpace(string(retryOutput)))
				}
			}
			return fmt.Errorf("nft DNS redirect fallback failed after iptables error %v: %w (output: %s)", err, nftErr, strings.TrimSpace(string(output)))
		}
		log.Infof("nft DNS redirect fallback installed successfully")
		return nil
	}
	return nil
}

func (r redirectRunner) rollbackRedirectRules(ctx context.Context, applied [][]string) error {
	var errs []string
	for i := len(applied) - 1; i >= 0; i-- {
		args := append([]string(nil), applied[i]...)
		args[3] = "-D"
		if output, err := r.runCommand(ctx, args); err != nil {
			if isIptablesMissingRuleError(output, err) {
				continue
			}
			errs = append(errs, fmt.Sprintf("%v (output: %s)", err, strings.TrimSpace(string(output))))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (r redirectRunner) removeNftRedirectTables(ctx context.Context) error {
	var errs []error
	for _, family := range dnsRedirectNftFamilies {
		script := fmt.Sprintf("delete table %s %s\n", family, dnsRedirectNftTable)
		if output, err := r.runNft(ctx, script); err != nil && !isNftMissingTableError(output, err) {
			errs = append(errs, fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(string(output))))
		}
	}
	if len(errs) > 0 {
		return nftCleanupError{errs: errs}
	}
	return nil
}

func isIptablesMissingRuleError(output []byte, err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error() + " " + string(output))
	return strings.Contains(msg, "bad rule") &&
		strings.Contains(msg, "matching rule")
}

func isNftUnavailableError(err error) bool {
	var cleanupErr nftCleanupError
	if errors.As(err, &cleanupErr) {
		for _, childErr := range cleanupErr.errs {
			if !isNftUnavailableError(childErr) {
				return false
			}
		}
		return len(cleanupErr.errs) > 0
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "protocol not supported") ||
		strings.Contains(msg, "operation not supported") ||
		strings.Contains(msg, "nf_tables") && strings.Contains(msg, "not supported")
}

func isNftMissingTableError(output []byte, err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error() + " " + string(output))
	return strings.Contains(msg, "no such file or directory") &&
		strings.Contains(msg, "delete table ") &&
		strings.Contains(msg, " "+dnsRedirectNftTable)
}

func removeNftDeleteTableLine(script string) string {
	var lines []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "delete table ") && strings.HasSuffix(line, " "+dnsRedirectNftTable) {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n") + "\n"
}

func isIptablesNftOutputAppendMissingChain(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "nf_tables") &&
		strings.Contains(msg, "rule_append failed") &&
		strings.Contains(msg, "no such file or directory") &&
		strings.Contains(msg, "chain output")
}

func dnsRedirectNftScript(port int, exemptDst []netip.Addr) string {
	var b strings.Builder
	fmt.Fprintf(&b, "delete table ip %s\n", dnsRedirectNftTable)
	fmt.Fprintf(&b, "delete table ip6 %s\n", dnsRedirectNftTable)
	fmt.Fprintf(&b, "add table ip %s\n", dnsRedirectNftTable)
	fmt.Fprintf(&b, "add chain ip %s output { type nat hook output priority -100; policy accept; }\n", dnsRedirectNftTable)
	fmt.Fprintf(&b, "add table ip6 %s\n", dnsRedirectNftTable)
	fmt.Fprintf(&b, "add chain ip6 %s output { type nat hook output priority -100; policy accept; }\n", dnsRedirectNftTable)
	for _, addr := range exemptDst {
		if addr.Is4() {
			fmt.Fprintf(&b, "add rule ip %s output udp dport 53 ip daddr %s return\n", dnsRedirectNftTable, addr.String())
			fmt.Fprintf(&b, "add rule ip %s output tcp dport 53 ip daddr %s return\n", dnsRedirectNftTable, addr.String())
		} else {
			fmt.Fprintf(&b, "add rule ip6 %s output udp dport 53 ip6 daddr %s return\n", dnsRedirectNftTable, addr.String())
			fmt.Fprintf(&b, "add rule ip6 %s output tcp dport 53 ip6 daddr %s return\n", dnsRedirectNftTable, addr.String())
		}
	}
	fmt.Fprintf(&b, "add rule ip %s output meta mark %s udp dport 53 return\n", dnsRedirectNftTable, constants.MarkHex)
	fmt.Fprintf(&b, "add rule ip %s output meta mark %s tcp dport 53 return\n", dnsRedirectNftTable, constants.MarkHex)
	fmt.Fprintf(&b, "add rule ip %s output udp dport 53 redirect to :%d\n", dnsRedirectNftTable, port)
	fmt.Fprintf(&b, "add rule ip %s output tcp dport 53 redirect to :%d\n", dnsRedirectNftTable, port)
	fmt.Fprintf(&b, "add rule ip6 %s output meta mark %s udp dport 53 return\n", dnsRedirectNftTable, constants.MarkHex)
	fmt.Fprintf(&b, "add rule ip6 %s output meta mark %s tcp dport 53 return\n", dnsRedirectNftTable, constants.MarkHex)
	fmt.Fprintf(&b, "add rule ip6 %s output udp dport 53 redirect to :%d\n", dnsRedirectNftTable, port)
	fmt.Fprintf(&b, "add rule ip6 %s output tcp dport 53 redirect to :%d\n", dnsRedirectNftTable, port)
	return b.String()
}

// SetupRedirect: OUTPUT 53 (udp/tcp) → port; sk_mark RETURN (proxy) and per-dst RETURN (exempt list) first.
func SetupRedirect(port int, exemptDst []netip.Addr) error {
	log.Infof("installing iptables DNS redirect: OUTPUT port 53 -> %d (mark %s bypass)", port, constants.MarkHex)
	if err := defaultRedirectRunner().setupRedirect(context.Background(), port, exemptDst); err != nil {
		return err
	}
	log.Infof("DNS redirect installed successfully")
	return nil
}

// RemoveRedirect deletes the same rules as SetupRedirect in reverse order; ignores missing rules.
func RemoveRedirect(port int, exemptDst []netip.Addr) {
	if err := defaultRedirectRunner().removeNftRedirectTables(context.Background()); err != nil {
		log.Warnf("nft DNS redirect remove table (ignored): %v", err)
	}
	rules := dnsRedirectRules(port, exemptDst, "-D")
	for i := len(rules) - 1; i >= 0; i-- {
		args := rules[i]
		if output, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			log.Warnf("iptables remove rule (ignored): %v (output: %s)", err, strings.TrimSpace(string(output)))
		}
	}
	log.Infof("iptables DNS redirect removed")
}
