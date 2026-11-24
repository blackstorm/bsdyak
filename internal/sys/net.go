package sys

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

const MaxIfNameLen = 15 // FreeBSD IFNAMSIZ - 1

type NetManager struct {
	runner Runner
}

func NewNetManager(runner Runner) *NetManager {
	return &NetManager{runner: runner}
}

func ValidateIfName(name string) error {
	if len(name) == 0 || len(name) > MaxIfNameLen {
		return fmt.Errorf("interface name %q exceeds FreeBSD IFNAMSIZ limit (%d chars)", name, MaxIfNameLen)
	}
	return nil
}

func (n *NetManager) EnsureBridge(ctx context.Context, bridgeName, gatewayCIDR string) error {
	if err := ValidateIfName(bridgeName); err != nil {
		return err
	}
	if _, err := n.runner.Run(ctx, "ifconfig", bridgeName); err == nil {
		return nil
	}
	res, err := n.runner.Run(ctx, "ifconfig", "bridge", "create", "name", bridgeName)
	if err != nil {
		// Fallback on older syntax: create then rename
		res, err = n.runner.Run(ctx, "ifconfig", "bridge", "create")
		if err != nil {
			return err
		}
		created := strings.TrimSpace(res.Stdout)
		if _, err := n.runner.Run(ctx, "ifconfig", created, "name", bridgeName); err != nil {
			return err
		}
	}
	_ = res
	if _, err := n.runner.Run(ctx, "ifconfig", bridgeName, "inet", gatewayCIDR, "up"); err != nil {
		return err
	}
	return nil
}

func (n *NetManager) DestroyInterface(ctx context.Context, iface string) error {
	if iface == "" {
		return nil
	}
	_, err := n.runner.Run(ctx, "ifconfig", iface, "destroy")
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "does not exist") {
		return nil
	}
	return err
}

// CreateEpair allocates a short <=11-char epair pair ("yk<8hex>a", "yk<8hex>b"),
// attaches human-readable description, adds the host end to bridgeName, and brings it up.
func (n *NetManager) CreateEpair(ctx context.Context, bridgeName, description string) (hostIf, jailIf string, err error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	id := hex.EncodeToString(raw[:])
	hostIf = "yk" + id + "a" // 11 chars <= 15
	jailIf = "yk" + id + "b" // 11 chars <= 15
	if err := ValidateIfName(hostIf); err != nil {
		return "", "", err
	}
	if err := ValidateIfName(jailIf); err != nil {
		return "", "", err
	}

	res, err := n.runner.Run(ctx, "ifconfig", "epair", "create")
	if err != nil {
		return "", "", err
	}
	createdA := strings.TrimSpace(res.Stdout)
	createdB := strings.TrimSuffix(createdA, "a") + "b"

	if _, err := n.runner.Run(ctx, "ifconfig", createdA, "name", hostIf); err != nil {
		return "", "", err
	}
	if _, err := n.runner.Run(ctx, "ifconfig", createdB, "name", jailIf); err != nil {
		_ = n.DestroyInterface(context.Background(), hostIf)
		return "", "", err
	}
	if description != "" {
		_, _ = n.runner.Run(ctx, "ifconfig", hostIf, "description", description)
		_, _ = n.runner.Run(ctx, "ifconfig", jailIf, "description", description)
	}
	if _, err := n.runner.Run(ctx, "ifconfig", bridgeName, "addm", hostIf); err != nil {
		_ = n.DestroyInterface(context.Background(), hostIf)
		return "", "", err
	}
	if _, err := n.runner.Run(ctx, "ifconfig", hostIf, "up"); err != nil {
		_ = n.DestroyInterface(context.Background(), hostIf)
		return "", "", err
	}
	return hostIf, jailIf, nil
}

// ApplyPFAnchor writes NAT and inter-project isolation rules strictly inside anchor "yak/<project>".
// Host /etc/pf.conf is never modified.
func (n *NetManager) ApplyPFAnchor(ctx context.Context, project, subnetCIDR, egressIface string, otherSubnets []string) error {
	if egressIface == "" {
		egressIface = "vtnet0"
	}
	var rules strings.Builder
	fmt.Fprintf(&rules, "nat on %s from %s to any -> (%s)\n", egressIface, subnetCIDR, egressIface)
	fmt.Fprintf(&rules, "pass quick from %s to %s\n", subnetCIDR, subnetCIDR)
	for _, other := range otherSubnets {
		if other != "" && other != subnetCIDR {
			fmt.Fprintf(&rules, "block drop quick from %s to %s\n", subnetCIDR, other)
		}
	}
	fmt.Fprintf(&rules, "pass quick from %s to any\n", subnetCIDR)

	anchor := "yak/" + project
	_, err := n.runner.RunWithInput(ctx, strings.NewReader(rules.String()), "pfctl", "-a", anchor, "-f", "-")
	return err
}

func (n *NetManager) FlushPFAnchor(ctx context.Context, project string) error {
	_, err := n.runner.Run(ctx, "pfctl", "-a", "yak/"+project, "-F", "all")
	return err
}
